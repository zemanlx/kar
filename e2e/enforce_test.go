//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// enMessage is the policy message that denials and warnings carry.
const enMessage = "deployments need an owner label"

var (
	errEnNotExempt = errors.New("namespace not exempt")
	errEnExempt    = errors.New("namespace still exempt")
	errEnDerived   = errors.New("derived binding differs")
	errEnNotDenied = errors.New("create not denied")
	errEnNoWarning = errors.New("warning missing")
	errEnCount     = errors.New("exempt count differs")
)

// enWarnings records every API server warning. It implements rest.WarningHandler.
type enWarnings struct {
	mu    sync.Mutex
	texts []string
}

// HandleWarningHeader records one warning.
func (w *enWarnings) HandleWarningHeader(_ int, _, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.texts = append(w.texts, text)
}

// take returns the warnings recorded since the last call and clears them.
func (w *enWarnings) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	texts := w.texts
	w.texts = nil

	return texts
}

// enWarnedClient returns a client that sends the API server's warnings to w.
func enWarnedClient(t *testing.T, w *enWarnings) client.Client {
	t.Helper()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}

	cfg.WarningHandler = w

	c, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	return c
}

// enOwnerless returns a Deployment named app2 with no owner label in namespace.
func enOwnerless(namespace string) *appsv1.Deployment {
	d := deployment(namespace, nil)
	d.Name = "app2"

	return d
}

// enDenied reports whether err is the owner policy rejecting a request.
func enDenied(err error) bool {
	return (apierrors.IsInvalid(err) || apierrors.IsForbidden(err)) && strings.Contains(err.Error(), enMessage)
}

// enExempt returns the namespaces listed in PolicyExemption name. A missing exemption lists none.
func enExempt(ctx context.Context, name string) ([]string, error) {
	pe, err := exemption(ctx, name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(pe.Status.Namespaces))
	for _, ns := range pe.Status.Namespaces {
		names = append(names, ns.Name)
	}

	return names, nil
}

// enFirstSeen returns when namespace was first recorded in the exemption name.
func enFirstSeen(ctx context.Context, name, namespace string) (metav1.Time, error) {
	pe, err := exemption(ctx, name)
	if err != nil {
		return metav1.Time{}, err
	}

	for _, ns := range pe.Status.Namespaces {
		if ns.Name == namespace {
			return ns.FirstSeen, nil
		}
	}

	return metav1.Time{}, fmt.Errorf("%w: %s", errEnNotExempt, namespace)
}

// enExemptGauge returns kar_exempt_namespaces for binding as the leader reports it.
func enExemptGauge(ctx context.Context, binding string) (float64, error) {
	lead, err := leader(ctx)
	if err != nil {
		return 0, err
	}

	text, err := metrics(ctx, lead.Name)
	if err != nil {
		return 0, err
	}

	return metricValue(text, "kar_exempt_namespaces", fmt.Sprintf("binding=%q", binding))
}

// enDryRunDenied returns an error unless a dry-run create of an ownerless Deployment in namespace is denied.
// A dry run leaves no object behind, so it is safe to retry while the API server loads the Deny binding.
func enDryRunDenied(ctx context.Context, namespace string) error {
	err := kube.Create(ctx, enOwnerless(namespace), client.DryRunAll)
	if enDenied(err) {
		return nil
	}

	if err == nil {
		return fmt.Errorf("%w in %s", errEnNotDenied, namespace)
	}

	return fmt.Errorf("dry run in %s: %w", namespace, err)
}

// enCheckAdmission waits for the Deny binding to load, then checks that an ownerless Deployment is admitted in the
// exempt namespace and denied in the other.
func enCheckAdmission(t *testing.T, f fixture) {
	t.Helper()

	ctx := t.Context()

	// Dry runs until the API server has loaded the Deny binding, so no object is left behind.
	eventually(t, time.Minute, func() error { return enDryRunDenied(ctx, f.Clean[0]) })

	if err := kube.Create(ctx, enOwnerless(f.Violating[0])); err != nil {
		t.Fatalf("create in exempt %s: %v", f.Violating[0], err)
	}

	if err := kube.Create(ctx, enOwnerless(f.Clean[0])); !enDenied(err) {
		t.Fatalf("create in %s: %v, want denial with %q", f.Clean[0], err, enMessage)
	}
}

// enExempted returns an error unless f.Violating[0] is exempt and f.Clean[0] is not.
func enExempted(ctx context.Context, f fixture) error {
	exempt, err := enExempt(ctx, f.Name)
	if err != nil {
		return err
	}

	if !slices.Contains(exempt, f.Violating[0]) {
		return fmt.Errorf("%w: %s", errEnNotExempt, f.Violating[0])
	}

	if slices.Contains(exempt, f.Clean[0]) {
		return fmt.Errorf("%w: %s", errEnExempt, f.Clean[0])
	}

	return nil
}

// enUnexempted returns an error unless namespace is gone from the exemption and from the derived binding's NotIn.
func enUnexempted(ctx context.Context, f fixture) error {
	exempt, err := enExempt(ctx, f.Name)
	if err != nil {
		return err
	}

	if slices.Contains(exempt, f.Violating[0]) {
		return fmt.Errorf("%w: %s", errEnExempt, f.Violating[0])
	}

	b, err := derived(ctx, f.Name)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return err
	}

	if slices.Contains(notIn(b), f.Violating[0]) {
		return fmt.Errorf("%w: derived NotIn still has %s", errEnDerived, f.Violating[0])
	}

	return nil
}

// enWarned reports whether any warning carries the owner policy message.
func enWarned(texts []string) bool {
	return slices.ContainsFunc(texts, func(s string) bool { return strings.Contains(s, enMessage) })
}

func TestObserve(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "observe", "", 1, 1)
	ctx := t.Context()

	eventually(t, 2*time.Minute, func() error { return enExempted(ctx, f) })

	if _, err := derived(ctx, f.Name); !apierrors.IsNotFound(err) {
		t.Fatalf("derived binding in observe mode: got %v, want NotFound", err)
	}

	warnings := &enWarnings{}
	c := enWarnedClient(t, warnings)
	attempt := 0

	// The Warn binding may not be loaded yet, so each attempt uses a new name rather than retrying a created one.
	eventually(t, 2*time.Minute, func() error {
		attempt++

		d := enOwnerless(f.Clean[0])
		d.Name = fmt.Sprintf("app2-%d", attempt)

		warnings.take()

		if err := c.Create(ctx, d); err != nil {
			return fmt.Errorf("create %s: %w", d.Name, err)
		}

		if !enWarned(warnings.take()) {
			return fmt.Errorf("%w: %s", errEnNoWarning, enMessage)
		}

		return nil
	})
}

func TestEnforceAdoption(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "enforce", "", 1, 1)
	ctx := t.Context()

	eventually(t, 2*time.Minute, func() error {
		b, err := derived(ctx, f.Name)
		if err != nil {
			return err
		}

		if !slices.Equal(notIn(b), f.Violating) {
			return fmt.Errorf("%w: NotIn %v, want %v", errEnDerived, notIn(b), f.Violating)
		}

		if want := []admissionregv1.ValidationAction{admissionregv1.Deny}; !slices.Equal(b.Spec.ValidationActions, want) {
			return fmt.Errorf("%w: actions %v, want %v", errEnDerived, b.Spec.ValidationActions, want)
		}

		return nil
	})

	enCheckAdmission(t, f)

	eventually(t, 2*time.Minute, func() error {
		got, err := enExemptGauge(ctx, f.Name)
		if err != nil {
			return err
		}

		if got != 1 {
			return fmt.Errorf("%w: %v, want 1", errEnCount, got)
		}

		return nil
	})
}

func TestObserveToEnforce(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "observe", "", 1, 1)
	ctx := t.Context()

	eventually(t, 2*time.Minute, func() error { return enExempted(ctx, f) })

	if _, err := derived(ctx, f.Name); !apierrors.IsNotFound(err) {
		t.Fatalf("derived binding in observe mode: got %v, want NotFound", err)
	}

	before, err := enFirstSeen(ctx, f.Name, f.Violating[0])
	if err != nil {
		t.Fatalf("first seen in observe mode: %v", err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var b admissionregv1.ValidatingAdmissionPolicyBinding
		if err := kube.Get(ctx, client.ObjectKey{Name: f.Name}, &b); err != nil {
			return fmt.Errorf("get source binding: %w", err)
		}

		b.Annotations[modeKey] = "enforce"

		if err := kube.Update(ctx, &b); err != nil {
			return fmt.Errorf("update source binding: %w", err)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("set %s to enforce: %v", modeKey, err)
	}

	eventually(t, 2*time.Minute, func() error {
		b, err := derived(ctx, f.Name)
		if err != nil {
			return err
		}

		if !slices.Equal(notIn(b), f.Violating) {
			return fmt.Errorf("%w: NotIn %v, want %v", errEnDerived, notIn(b), f.Violating)
		}

		return nil
	})

	// The status keeps the exemption across the mode change, so the first sighting must not move.
	after, err := enFirstSeen(ctx, f.Name, f.Violating[0])
	if err != nil {
		t.Fatalf("first seen in enforce mode: %v", err)
	}

	if !before.Equal(&after) {
		t.Fatalf("first seen of %s changed from %v to %v", f.Violating[0], before, after)
	}

	enCheckAdmission(t, f)
}

func TestTTLExpiry(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "enforce", "10s", 1, 0)
	ctx := t.Context()

	eventually(t, 2*time.Minute, func() error {
		b, err := derived(ctx, f.Name)
		if err != nil {
			return err
		}

		if !slices.Equal(notIn(b), f.Violating) {
			return fmt.Errorf("%w: NotIn %v, want %v", errEnDerived, notIn(b), f.Violating)
		}

		return nil
	})

	var dep appsv1.Deployment
	if err := kube.Get(ctx, client.ObjectKey{Namespace: f.Violating[0], Name: "app"}, &dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	metav1.SetMetaDataLabel(&dep.ObjectMeta, "owner", "e2e")

	if err := kube.Update(ctx, &dep); err != nil {
		t.Fatalf("label deployment: %v", err)
	}

	// Only a later audit pass notices the fix, and the 10s TTL then expires the exemption.
	eventually(t, 3*time.Minute, func() error { return enUnexempted(ctx, f) })

	eventually(t, time.Minute, func() error { return enDryRunDenied(ctx, f.Violating[0]) })

	if err := kube.Create(ctx, enOwnerless(f.Violating[0])); !enDenied(err) {
		t.Fatalf("create in expired %s: %v, want denial with %q", f.Violating[0], err, enMessage)
	}
}
