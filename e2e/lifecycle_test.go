//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	karv1alpha1 "github.com/zemanlx/kar/api/v1alpha1"
)

// lcUnauditable reads oldObject, which a CREATE-only policy cannot audit.
const lcUnauditable = "oldObject == null || (has(object.metadata.labels) && 'owner' in object.metadata.labels)"

var (
	errLcExists     = errors.New("object still exists")
	errLcNotIn      = errors.New("derived notIn differs")
	errLcListed     = errors.New("exemption still lists namespace")
	errLcPods       = errors.New("kar pods still exist")
	errLcNoCond     = errors.New("no Auditable condition yet")
	errLcCond       = errors.New("condition Auditable not False")
	errLcNotReady   = errors.New("kar not ready")
	errLcNotRunning = errors.New("leader pod not running")
)

func TestNamespaceDeletion(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "enforce", "", 2, 0)

	eventually(t, 2*time.Minute, func() error {
		b, err := derived(t.Context(), f.Name)
		if err != nil {
			return err
		}

		if got := notIn(b); !slices.Contains(got, f.Violating[0]) || !slices.Contains(got, f.Violating[1]) {
			return fmt.Errorf("%w: %v, want %v", errLcNotIn, got, f.Violating)
		}

		return nil
	})

	ns0 := &corev1.Namespace{Name: f.Violating[0]}
	if err := kube.Delete(t.Context(), ns0); err != nil {
		t.Fatalf("delete namespace %s: %v", ns0.Name, err)
	}

	eventually(t, 3*time.Minute, func() error {
		pe, err := exemption(t.Context(), f.Name)
		if err != nil {
			return err
		}

		for _, n := range pe.Status.Namespaces {
			if n.Name == f.Violating[0] {
				return fmt.Errorf("%w: %s", errLcListed, n.Name)
			}
		}

		b, err := derived(t.Context(), f.Name)
		if err != nil {
			return err
		}

		if got := notIn(b); !slices.Equal(got, []string{f.Violating[1]}) {
			return fmt.Errorf("%w: %v, want [%s]", errLcNotIn, got, f.Violating[1])
		}

		return nil
	})
}

func TestOptOut(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "enforce", "", 1, 0)

	eventually(t, 2*time.Minute, func() error { return lcPresent(t.Context(), f.Name) })

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var b admissionregv1.ValidatingAdmissionPolicyBinding
		if err := kube.Get(t.Context(), client.ObjectKey{Name: f.Name}, &b); err != nil {
			return fmt.Errorf("get source binding: %w", err)
		}

		delete(b.Annotations, modeKey)

		if err := kube.Update(t.Context(), &b); err != nil {
			return fmt.Errorf("update source binding: %w", err)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("remove %s annotation: %v", modeKey, err)
	}

	eventually(t, time.Minute, func() error { return lcAbsent(t.Context(), f.Name) })
}

//nolint:paralleltest // scales the shared kar Deployment, so it must run alone
func TestGarbageCollection(t *testing.T) {
	f := newFixture(t, "enforce", "", 1, 0)

	eventually(t, 2*time.Minute, func() error { return lcPresent(t.Context(), f.Name) })

	// Registered before scaling, so kar comes back even if a step below fails. Cleanups run LIFO, so this runs
	// before the fixture's cleanup.
	t.Cleanup(func() { lcRestoreKar(t) })

	if err := lcScaleKar(t.Context(), 0); err != nil {
		t.Fatalf("scale kar to 0: %v", err)
	}

	eventually(t, 2*time.Minute, func() error {
		running, err := karPods(t.Context())
		if err != nil {
			return err
		}

		if len(running) != 0 {
			return fmt.Errorf("%w: %d running", errLcPods, len(running))
		}

		n, err := lcKarPodCount(t.Context())
		if err != nil {
			return err
		}

		if n != 0 {
			return fmt.Errorf("%w: %d", errLcPods, n)
		}

		return nil
	})

	src := &admissionregv1.ValidatingAdmissionPolicyBinding{Name: f.Name}
	if err := kube.Delete(t.Context(), src, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatalf("delete source binding %s: %v", f.Name, err)
	}

	eventually(t, 2*time.Minute, func() error { return lcAbsent(t.Context(), f.Name) })
}

func TestNotAuditable(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "enforce", "", 1, 0)

	eventually(t, time.Minute, func() error {
		_, err := derived(t.Context(), f.Name)

		return err
	})

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var vap admissionregv1.ValidatingAdmissionPolicy
		if err := kube.Get(t.Context(), client.ObjectKey{Name: f.Name}, &vap); err != nil {
			return fmt.Errorf("get policy: %w", err)
		}

		vap.Spec.Validations[0].Expression = lcUnauditable

		if err := kube.Update(t.Context(), &vap); err != nil {
			return fmt.Errorf("update policy: %w", err)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("update policy %s: %v", f.Name, err)
	}

	eventually(t, 2*time.Minute, func() error {
		pe, err := exemption(t.Context(), f.Name)
		if err != nil {
			return err
		}

		c := meta.FindStatusCondition(pe.Status.Conditions, karv1alpha1.ConditionAuditable)
		if c == nil {
			return errLcNoCond
		}

		if c.Status != metav1.ConditionFalse {
			return fmt.Errorf("%w: %s", errLcCond, c.Status)
		}

		return nil
	})

	eventually(t, time.Minute, func() error {
		_, err := derived(t.Context(), f.Name)

		return lcGone(err)
	})
}

// lcPresent returns an error unless the derived binding and the exemption for source both exist.
func lcPresent(ctx context.Context, source string) error {
	if _, err := derived(ctx, source); err != nil {
		return err
	}

	_, err := exemption(ctx, source)

	return err
}

// lcAbsent returns an error unless the derived binding and the exemption for source are both NotFound.
func lcAbsent(ctx context.Context, source string) error {
	_, derr := derived(ctx, source)
	_, eerr := exemption(ctx, source)

	return errors.Join(lcGone(derr), lcGone(eerr))
}

// lcGone returns nil when err is NotFound. A nil err means the object exists, so it is an error too.
func lcGone(err error) error {
	if err == nil {
		return errLcExists
	}

	if apierrors.IsNotFound(err) {
		return nil
	}

	return err
}

// lcScaleKar sets the kar Deployment's replicas with a merge patch.
func lcScaleKar(ctx context.Context, n int) error {
	dep := &appsv1.Deployment{Namespace: ns, Name: karName}
	patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, n)

	if err := kube.Patch(ctx, dep, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("patch kar: %w", err)
	}

	return nil
}

// lcRestoreKar scales kar back to replicas and waits until it is ready and a running pod holds the lease.
func lcRestoreKar(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := lcScaleKar(ctx, replicas); err != nil {
		t.Errorf("scale kar to %d: %v", replicas, err)

		return
	}

	var last error

	_ = wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		last = lcKarReady(ctx)

		return last == nil, nil
	})

	if last != nil {
		t.Errorf("kar not ready after scale-up: %v", last)
	}
}

// lcKarReady returns an error until kar has all replicas ready and the lease holder is a running pod.
func lcKarReady(ctx context.Context) error {
	var dep appsv1.Deployment
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ns, Name: karName}, &dep); err != nil {
		return fmt.Errorf("get kar deployment: %w", err)
	}

	if dep.Status.ReadyReplicas != replicas {
		return fmt.Errorf("%w: %d ready, want %d", errLcNotReady, dep.Status.ReadyReplicas, replicas)
	}

	pod, err := leader(ctx)
	if err != nil {
		return err
	}

	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("%w: %s is %s", errLcNotRunning, pod.Name, pod.Status.Phase)
	}

	return nil
}

// lcKarPodCount counts kar pods in any phase, terminating ones included.
func lcKarPodCount(ctx context.Context) (int, error) {
	var list corev1.PodList
	if err := kube.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{"app.kubernetes.io/name": karName}); err != nil {
		return 0, fmt.Errorf("list kar pods: %w", err)
	}

	return len(list.Items), nil
}
