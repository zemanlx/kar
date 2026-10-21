package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	karv1alpha1 "github.com/zemanlx/kar/api/v1alpha1"
	"github.com/zemanlx/kat/evaluator"
)

const (
	envTTL     = 192 * time.Hour // the default TTL main.go uses
	sourceName = "require-owner" // the source binding and policy of the fixtures
)

var (
	errNotRecorded    = errors.New("namespace missing from status.namespaces")
	errExpiration     = errors.New("expiration is not about now plus the TTL")
	errStillRecorded  = errors.New("namespace still in status.namespaces")
	errNotDenied      = errors.New("violation was not denied")
	errMismatch       = errors.New("unexpected state")
	errStillDerived   = errors.New("derived binding not removed")
	errNotEstablished = errors.New("CRD not established")
)

func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(karv1alpha1.AddToScheme(scheme))

	return scheme
}

// startAPIServer starts kube-apiserver and etcd via envtest, with etcdFlags added to etcd's command line.
// It skips when KUBEBUILDER_ASSETS is unset.
func startAPIServer(t *testing.T, etcdFlags map[string]string) *rest.Config {
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; envtest needs kube-apiserver and etcd")
	}

	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{Paths: []string{"../../charts/kar/crds/kar.zeman.dev_policyexemptions.yaml"}}}
	env.ControlPlane.Etcd = &envtest.Etcd{}

	for flag, value := range etcdFlags {
		env.ControlPlane.Etcd.Configure().Set(flag, value)
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}

	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	return cfg
}

func newClient(t *testing.T, cfg *rest.Config) client.Client {
	t.Helper()

	c, err := client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	return c
}

// startEnv starts the API server and the audit controller, and returns a direct client for assertions.
func startEnv(t *testing.T) client.Client {
	t.Helper()

	cfg := startAPIServer(t, nil)
	startController(t, cfg)

	return newClient(t, cfg)
}

// startController runs the audit controller against cfg until t's cleanup, and returns a stop func
// that ends it early. Options mirror main.go's manager, minus probes and leader election.
func startController(t *testing.T, cfg *rest.Config) (stop func()) {
	t.Helper()

	cfg = rest.CopyConfig(cfg)
	cfg.QPS = -1 // as ctrl.GetConfig sets it: no client-side throttling

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     newScheme(),
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Client:     client.Options{Cache: &client.CacheOptions{Unstructured: true}},
		Cache:      cache.Options{DefaultTransform: cache.TransformStripManagedFields()},
		Controller: config.Controller{SkipNameValidation: new(true)}, // every test starts a manager named kar
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}

	ev, err := evaluator.New()
	if err != nil {
		t.Fatalf("create evaluator: %v", err)
	}

	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatalf("create discovery client: %v", err)
	}

	rec := &Reconciler{Client: mgr.GetClient(), Cache: mgr.GetCache(), Discovery: memory.NewMemCacheClient(dc), Evaluator: ev, DefaultTTL: envTTL}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("set up controller: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- mgr.Start(ctx) }()

	// Cleanups run last-in first-out, so the manager stops before envtest does.
	stop = sync.OnceFunc(func() {
		cancel()

		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	t.Cleanup(stop)

	return stop
}

// eventually polls f every 100ms until it returns nil or timeout passes, then fails with the last error.
func eventually(t *testing.T, timeout time.Duration, f func() error) {
	t.Helper()

	var last error

	err := wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, timeout, true, func(context.Context) (bool, error) {
		last = f()

		return last == nil, nil
	})
	if err != nil {
		t.Fatalf("condition not met within %s: %v", timeout, last)
	}
}

// violation is a ConfigMap that the require-owner policy denies, as it has no owner label.
func violation(ns string) *corev1.ConfigMap {
	return &corev1.ConfigMap{Name: "app", Namespace: ns}
}

// fixtureNS is a namespace the require-owner policy applies to. Other namespaces, such as kube-system
// with its own unlabeled ConfigMaps, are out of scope.
func fixtureNS(name string) *corev1.Namespace {
	return &corev1.Namespace{Name: name, Labels: map[string]string{"kar-test": "fixture"}}
}

// violatingFixture is a violating ConfigMap in bad and the require-owner policy that denies it, with a
// binding carrying annotations. The objects come first, so the first audit pass sees the ConfigMap
// without waiting for a resync.
func violatingFixture(annotations map[string]string) []client.Object {
	return []client.Object{
		fixtureNS("bad"),
		violation("bad"),
		&admissionregv1.ValidatingAdmissionPolicy{
			Name: sourceName,
			Spec: admissionregv1.ValidatingAdmissionPolicySpec{
				MatchConstraints: &admissionregv1.MatchResources{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kar-test": "fixture"}},
					ResourceRules: []admissionregv1.NamedRuleWithOperations{{
						Operations:  []admissionregv1.OperationType{admissionregv1.Create, admissionregv1.Update},
						APIGroups:   []string{""},
						APIVersions: []string{"v1"},
						Resources:   []string{"configmaps"},
					}},
				},
				Validations: []admissionregv1.Validation{{Expression: "has(object.metadata.labels) && 'owner' in object.metadata.labels"}},
			},
		},
		&admissionregv1.ValidatingAdmissionPolicyBinding{
			Name:        sourceName,
			Annotations: annotations,
			Spec: admissionregv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName:        sourceName,
				ValidationActions: []admissionregv1.ValidationAction{admissionregv1.Warn},
			},
		},
	}
}

func createAll(t *testing.T, c client.Client, objs []client.Object) {
	t.Helper()

	for _, obj := range objs {
		if err := c.Create(t.Context(), obj); err != nil {
			t.Fatalf("create %s: %v", obj.GetName(), err)
		}
	}
}

// recorded returns an error unless bad is in the PolicyExemption status.
func recorded(ctx context.Context, c client.Client) error {
	pe := &karv1alpha1.PolicyExemption{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
		return fmt.Errorf("get exemption: %w", err)
	}

	for _, e := range pe.Status.Namespaces {
		if e.Name == "bad" {
			return nil
		}
	}

	return fmt.Errorf("%w: %v", errNotRecorded, pe.Status.Namespaces)
}

// recordedAll returns an error unless every namespace in want is in the PolicyExemption status.
func recordedAll(ctx context.Context, c client.Client, want []string) error {
	pe := &karv1alpha1.PolicyExemption{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
		return fmt.Errorf("get exemption: %w", err)
	}

	got := make(map[string]bool, len(pe.Status.Namespaces))
	for _, e := range pe.Status.Namespaces {
		got[e.Name] = true
	}

	var missing []string

	for _, ns := range want {
		if !got[ns] {
			missing = append(missing, ns)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("%w: %v", errNotRecorded, missing)
	}

	return nil
}

// derivedIs returns an error unless the derived binding is Deny and exempts exactly want.
func derivedIs(ctx context.Context, c client.Client, want []string) error {
	b := &admissionregv1.ValidatingAdmissionPolicyBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: derivedName(sourceName)}, b); err != nil {
		return fmt.Errorf("get derived binding: %w", err)
	}

	if got := notIn(b); !slices.Equal(got, want) {
		return fmt.Errorf("%w: NotIn %v, want %v", errMismatch, got, want)
	}

	if !slices.Equal(b.Spec.ValidationActions, []admissionregv1.ValidationAction{admissionregv1.Deny}) {
		return fmt.Errorf("%w: validationActions %v, want [Deny]", errMismatch, b.Spec.ValidationActions)
	}

	return nil
}

// notIn returns the values of b's metadata.name NotIn expression, or nil when it has none.
func notIn(b *admissionregv1.ValidatingAdmissionPolicyBinding) []string {
	if b.Spec.MatchResources == nil || b.Spec.MatchResources.NamespaceSelector == nil {
		return nil
	}

	for _, e := range b.Spec.MatchResources.NamespaceSelector.MatchExpressions {
		if e.Key == corev1.LabelMetadataName && e.Operator == metav1.LabelSelectorOpNotIn {
			return e.Values
		}
	}

	return nil
}

// isDenied reports whether err is the admission rejection of a violating object.
func isDenied(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err)
}

func TestObserveRecordsViolatingNamespace(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeObserve}))

	eventually(t, time.Minute, func() error {
		pe := &karv1alpha1.PolicyExemption{}
		if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
			return fmt.Errorf("get exemption: %w", err)
		}

		for _, ns := range pe.Status.Namespaces {
			if ns.Name != "bad" {
				continue
			}

			// FirstSeen is at or after the test start, so the expiration is at most a few seconds early.
			want := time.Now().Add(envTTL)
			if d := ns.Expiration.Sub(want); d < -time.Minute || d > time.Minute {
				return fmt.Errorf("%w: got %s, want about %s", errExpiration, ns.Expiration, want)
			}

			return nil
		}

		return fmt.Errorf("%w: %v", errNotRecorded, pe.Status.Namespaces)
	})

	// Observe mode never derives an enforcing binding.
	if err := c.Get(ctx, client.ObjectKey{Name: derivedName(sourceName)}, &admissionregv1.ValidatingAdmissionPolicyBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("%s: want NotFound, got %v", derivedName(sourceName), err)
	}
}

func TestEnforceDeniesOutsideExemptNamespaces(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, append(violatingFixture(map[string]string{modeAnnotation: modeEnforce}), fixtureNS("good")))

	eventually(t, time.Minute, func() error { return derivedIs(ctx, c, []string{"bad"}) })

	// The VAP takes a moment to become active, so poll with dry runs until a violation in good is denied.
	eventually(t, 30*time.Second, func() error {
		err := c.Create(ctx, violation("good"), client.DryRunAll)
		if isDenied(err) {
			return nil
		}

		return fmt.Errorf("%w in good: got %w", errNotDenied, err)
	})

	if err := c.Create(ctx, violation("good")); !isDenied(err) {
		t.Fatalf("violation in good: %v", err)
	}

	if err := c.Create(ctx, &corev1.ConfigMap{Name: "other", Namespace: "bad"}); err != nil {
		t.Fatalf("violation in exempt bad: %v", err)
	}
}

// TestEnforceLongSourceName checks that a source name too long for a label, or for a derived name
// without a hash, still gets its Deny binding.
func TestEnforceLongSourceName(t *testing.T) {
	t.Parallel()

	for _, n := range []int{64, 250} {
		t.Run(fmt.Sprintf("%d characters", n), func(t *testing.T) {
			t.Parallel()

			c := startEnv(t)
			ctx := t.Context()
			name := strings.Repeat("a", n)

			objs := violatingFixture(map[string]string{modeAnnotation: modeEnforce})
			objs[len(objs)-1].SetName(name) // the source binding, which still names the policy
			createAll(t, c, objs)

			eventually(t, time.Minute, func() error {
				pe := &karv1alpha1.PolicyExemption{}
				if err := c.Get(ctx, client.ObjectKey{Name: name}, pe); err != nil {
					return fmt.Errorf("get exemption: %w", err)
				}

				cond := meta.FindStatusCondition(pe.Status.Conditions, karv1alpha1.ConditionEnforcing)
				if cond == nil || cond.Status != metav1.ConditionTrue {
					return fmt.Errorf("%w: Enforcing %v", errMismatch, cond)
				}

				return nil
			})

			b := &admissionregv1.ValidatingAdmissionPolicyBinding{}
			if err := c.Get(ctx, client.ObjectKey{Name: derivedName(name)}, b); err != nil {
				t.Fatalf("get derived binding: %v", err)
			}

			if got := notIn(b); !slices.Equal(got, []string{"bad"}) {
				t.Errorf("NotIn %v, want [bad]", got)
			}
		})
	}
}

func TestTTLExpiry(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("waits for a resync pass to expire the exemption")
	}

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce, ttlAnnotation: "1s"}))

	eventually(t, time.Minute, func() error { return recorded(ctx, c) })

	// Fix the ConfigMap. Only the next pass notices the expiry.
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "bad", Name: "app"}, cm); err != nil {
		t.Fatalf("get configmap: %v", err)
	}

	cm.Labels = map[string]string{"owner": "team"}
	if err := c.Update(ctx, cm); err != nil {
		t.Fatalf("label configmap: %v", err)
	}

	eventually(t, 150*time.Second, func() error {
		if recorded(ctx, c) == nil {
			return errStillRecorded
		}

		return derivedIs(ctx, c, nil)
	})
}

// TestShortTTLStatusTracksExpiration checks that a binding with a TTL under statusStep still gets its
// status expiration refreshed while the namespace keeps violating, so a new leader does not load an
// expiration that is already past.
func TestShortTTLStatusTracksExpiration(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("waits for a resync pass to refresh the expiration")
	}

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeObserve, ttlAnnotation: "30s"}))

	eventually(t, time.Minute, func() error { return recorded(ctx, c) })

	first, err := statusExpiration(ctx, c, "bad")
	if err != nil {
		t.Fatal(err)
	}

	eventually(t, 100*time.Second, func() error {
		got, err := statusExpiration(ctx, c, "bad")
		if err != nil {
			return err
		}

		if !got.After(first) {
			return fmt.Errorf("%w: status expiration still %s", errExpiration, got)
		}

		return nil
	})
}

// observingSince returns the observation clock of the exemption and the inputs it started under. The clock is
// zero when it has not started.
func observingSince(ctx context.Context, c client.Client) (time.Time, string, error) {
	pe := &karv1alpha1.PolicyExemption{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
		return time.Time{}, "", fmt.Errorf("get exemption: %w", err)
	}

	var since time.Time
	if pe.Status.ObservingSince != nil {
		since = pe.Status.ObservingSince.Time
	}

	return since, pe.Status.ObservedInputs, nil
}

// waitClock waits up to a minute for the observation clock to start, and returns it with the inputs it started under.
func waitClock(t *testing.T, c client.Client) (time.Time, string) {
	t.Helper()

	var (
		since  time.Time
		inputs string
	)

	eventually(t, time.Minute, func() error {
		var err error

		since, inputs, err = observingSince(t.Context(), c)
		if err != nil {
			return err
		}

		if since.IsZero() {
			return fmt.Errorf("%w: observingSince unset", errMismatch)
		}

		return nil
	})

	return since, inputs
}

// enforcingIs returns an error unless the Enforcing condition of the exemption has status and reason.
func enforcingIs(ctx context.Context, c client.Client, status metav1.ConditionStatus, reason string) error {
	pe := &karv1alpha1.PolicyExemption{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
		return fmt.Errorf("get exemption: %w", err)
	}

	cond := meta.FindStatusCondition(pe.Status.Conditions, karv1alpha1.ConditionEnforcing)
	if cond == nil || cond.Status != status || cond.Reason != reason {
		return fmt.Errorf("%w: Enforcing %+v, want %s %s", errMismatch, cond, status, reason)
	}

	return nil
}

func TestAutoSwitchesAfterTTL(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("waits for the TTL to pass")
	}

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeAuto, ttlAnnotation: "10s"}))
	waitClock(t, c)

	eventually(t, time.Minute, func() error {
		if err := recorded(ctx, c); err != nil {
			return err
		}

		return enforcingIs(ctx, c, metav1.ConditionFalse, karv1alpha1.ReasonObserve)
	})

	if err := c.Get(ctx, client.ObjectKey{Name: derivedName(sourceName)}, &admissionregv1.ValidatingAdmissionPolicyBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("%s: want NotFound while observing, got %v", derivedName(sourceName), err)
	}

	eventually(t, time.Minute, func() error {
		if err := derivedIs(ctx, c, []string{"bad"}); err != nil {
			return err
		}

		return enforcingIs(ctx, c, metav1.ConditionTrue, karv1alpha1.ReasonEnforcing)
	})
}

func TestAutoRestartKeepsClock(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("waits for the TTL to pass")
	}

	cfg := startAPIServer(t, nil)
	c := newClient(t, cfg)
	ctx := t.Context()
	stop := startController(t, cfg)

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeAuto, ttlAnnotation: "15s"}))

	since, _ := waitClock(t, c)

	stop()
	startController(t, cfg)

	eventually(t, time.Minute, func() error { return derivedIs(ctx, c, []string{"bad"}) })

	got, _, err := observingSince(ctx, c)
	if err != nil {
		t.Fatal(err)
	}

	if !got.Equal(since) {
		t.Fatalf("observingSince %s after restart, want %s", got, since)
	}
}

func TestAutoPolicyChangeResetsClock(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeAuto, ttlAnnotation: "1h"}))

	since, inputs := waitClock(t, c)

	// The clock is kept to whole seconds, so wait past one before a reset can show.
	time.Sleep(1100 * time.Millisecond)

	p := &admissionregv1.ValidatingAdmissionPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, p); err != nil {
		t.Fatalf("get policy: %v", err)
	}

	p.Spec.Validations[0].Message = "changed"

	if err := c.Update(ctx, p); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	eventually(t, time.Minute, func() error {
		got, gotInputs, err := observingSince(ctx, c)
		if err != nil {
			return err
		}

		if gotInputs == inputs {
			return fmt.Errorf("%w: observedInputs still %q", errMismatch, inputs)
		}

		if !got.After(since) {
			return fmt.Errorf("%w: observingSince %s, want after %s", errMismatch, got, since)
		}

		return nil
	})

	if err := c.Get(ctx, client.ObjectKey{Name: derivedName(sourceName)}, &admissionregv1.ValidatingAdmissionPolicyBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("%s: want NotFound after the reset, got %v", derivedName(sourceName), err)
	}
}

func TestAutoLatchSurvivesTTLRaise(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("waits for the TTL to pass")
	}

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeAuto, ttlAnnotation: "3s"}))

	eventually(t, time.Minute, func() error { return derivedIs(ctx, c, []string{"bad"}) })

	b := &admissionregv1.ValidatingAdmissionPolicyBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, b); err != nil {
		t.Fatalf("get binding: %v", err)
	}

	b.Annotations[ttlAnnotation] = "1h"

	if err := c.Update(ctx, b); err != nil {
		t.Fatalf("raise ttl: %v", err)
	}

	// The raised TTL puts the deadline in the future, so only the latch keeps the binding enforcing.
	for range 20 {
		if err := derivedIs(ctx, c, []string{"bad"}); err != nil {
			t.Fatalf("derived binding lost after the TTL was raised: %v", err)
		}

		time.Sleep(500 * time.Millisecond)
	}
}

func TestObserveToAutoCountsEarlierTime(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("waits for the TTL to pass")
	}

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeObserve, ttlAnnotation: "5s"}))

	since, _ := waitClock(t, c)

	// Switch once the observation window is over. The clock started in observe must carry over.
	time.Sleep(time.Until(since.Add(6 * time.Second)))

	b := &admissionregv1.ValidatingAdmissionPolicyBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, b); err != nil {
		t.Fatalf("get binding: %v", err)
	}

	b.Annotations[modeAnnotation] = modeAuto

	if err := c.Update(ctx, b); err != nil {
		t.Fatalf("switch to auto: %v", err)
	}

	eventually(t, 15*time.Second, func() error { return derivedIs(ctx, c, []string{"bad"}) })

	got, _, err := observingSince(ctx, c)
	if err != nil {
		t.Fatal(err)
	}

	if !got.Equal(since) {
		t.Fatalf("observingSince %s after the switch, want %s", got, since)
	}
}

// statusExpiration returns ns's expiration as the PolicyExemption status records it.
func statusExpiration(ctx context.Context, c client.Client, ns string) (time.Time, error) {
	pe := &karv1alpha1.PolicyExemption{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
		return time.Time{}, fmt.Errorf("get exemption: %w", err)
	}

	for _, e := range pe.Status.Namespaces {
		if e.Name == ns {
			return e.Expiration.Time, nil
		}
	}

	return time.Time{}, fmt.Errorf("%w: %s", errNotRecorded, ns)
}

func TestRestartRecoversFromBinding(t *testing.T) {
	t.Parallel()

	cfg := startAPIServer(t, nil)
	c := newClient(t, cfg)
	ctx := t.Context()
	stop := startController(t, cfg)

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce}))

	eventually(t, time.Minute, func() error {
		if err := recorded(ctx, c); err != nil {
			return err
		}

		return derivedIs(ctx, c, []string{"bad"})
	})

	stop()

	// Clear the status and delete the violation, so only the derived binding can bring bad back.
	pe := &karv1alpha1.PolicyExemption{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
		t.Fatalf("get exemption: %v", err)
	}

	pe.Status.Namespaces = nil
	if err := c.Status().Update(ctx, pe); err != nil {
		t.Fatalf("clear status: %v", err)
	}

	if err := c.Delete(ctx, violation("bad")); err != nil {
		t.Fatalf("delete violation: %v", err)
	}

	startController(t, cfg)

	eventually(t, time.Minute, func() error {
		if err := recorded(ctx, c); err != nil {
			return err
		}

		return derivedIs(ctx, c, []string{"bad"})
	})
}

// TestStateTooLarge runs etcd with a 64 KiB request limit. Once bad is exempt, labelling 600 violating
// namespaces into scope makes the state too large for etcd. kar must keep the last state that fit and
// leave the Deny binding exempting exactly that state.
func TestStateTooLarge(t *testing.T) {
	t.Parallel()

	const n = 600

	cfg := startAPIServer(t, map[string]string{"max-request-bytes": "65536"})
	c := newClient(t, cfg)
	ctx := t.Context()

	startController(t, cfg)

	// Out of scope until labelled, so the Deny binding does not reject their ConfigMaps.
	overflow := make([]*corev1.Namespace, 0, n)

	for i := range n {
		ns := &corev1.Namespace{Name: fmt.Sprintf("overflow-%054d", i)}
		overflow = append(overflow, ns)
		createAll(t, c, []client.Object{ns, violation(ns.Name)})
	}

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce}))
	eventually(t, time.Minute, func() error { return derivedIs(ctx, c, []string{"bad"}) })

	for _, ns := range overflow {
		ns.Labels = fixtureNS(ns.Name).Labels
		if err := c.Update(ctx, ns); err != nil {
			t.Fatalf("label %s: %v", ns.Name, err)
		}
	}

	pe := &karv1alpha1.PolicyExemption{}

	eventually(t, time.Minute, func() error {
		if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
			return fmt.Errorf("get exemption: %w", err)
		}

		return tooLargeErr(pe)
	})

	kept := make([]string, 0, len(pe.Status.Namespaces))
	for _, e := range pe.Status.Namespaces {
		kept = append(kept, e.Name)
	}

	if len(kept) > n || !slices.Contains(kept, "bad") {
		t.Fatalf("status kept %d namespaces, want bad and fewer than %d", len(kept), n+1)
	}

	if err := derivedIs(ctx, c, kept); err != nil {
		t.Fatal(err)
	}
}

// TestBulkRelabelIsFast labels 1000 violating namespaces into scope at once. Each must be exempt within 15
// seconds of the last label, so the passes that exempt them do not queue up behind one another.
func TestBulkRelabelIsFast(t *testing.T) {
	t.Parallel()

	const n = 1000

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce}))
	eventually(t, time.Minute, func() error { return derivedIs(ctx, c, []string{"bad"}) })

	// Out of scope until labelled, so the Deny binding does not reject their ConfigMaps.
	want := make([]string, 0, n+1)
	want = append(want, "bad")
	bulk := make([]*corev1.Namespace, 0, n)

	for i := range n {
		ns := &corev1.Namespace{Name: fmt.Sprintf("bulk-%058d", i)}
		bulk = append(bulk, ns)
		want = append(want, ns.Name)
		createAll(t, c, []client.Object{ns, violation(ns.Name)})
	}

	slices.Sort(want)

	for _, ns := range bulk {
		ns.Labels = fixtureNS(ns.Name).Labels
		if err := c.Update(ctx, ns); err != nil {
			t.Fatalf("label %s: %v", ns.Name, err)
		}
	}

	eventually(t, 15*time.Second, func() error { return derivedIs(ctx, c, want) })
}

// tooLargeErr returns an error unless pe's Synced condition is False with reason StateTooLarge and carries
// the server's error.
func tooLargeErr(pe *karv1alpha1.PolicyExemption) error {
	cond := meta.FindStatusCondition(pe.Status.Conditions, karv1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != karv1alpha1.ReasonStateTooLarge {
		return fmt.Errorf("%w: Synced %+v, want False %s", errMismatch, cond, karv1alpha1.ReasonStateTooLarge)
	}

	if !strings.Contains(cond.Message, "too large") {
		return fmt.Errorf("%w: Synced message %q does not carry the server error", errMismatch, cond.Message)
	}

	return nil
}

func TestShortLivedViolationIsRecorded(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	// Observe mode, so the violation can be created even though the policy denies it. Without the
	// ConfigMap the informer still starts with the binding.
	fixture := violatingFixture(map[string]string{modeAnnotation: modeObserve})
	createAll(t, c, slices.Concat(fixture[:1], fixture[2:]))

	// The exemption is Synced once a pass has run, and the informer is running by then.
	eventually(t, time.Minute, func() error {
		pe := &karv1alpha1.PolicyExemption{}
		if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
			return fmt.Errorf("get exemption: %w", err)
		}

		if !meta.IsStatusConditionTrue(pe.Status.Conditions, karv1alpha1.ConditionSynced) {
			return fmt.Errorf("%w: conditions %+v, want Synced True", errMismatch, pe.Status.Conditions)
		}

		return nil
	})

	createAll(t, c, []client.Object{violation("bad")})

	if err := c.Delete(ctx, violation("bad")); err != nil {
		t.Fatalf("delete violation: %v", err)
	}

	eventually(t, time.Minute, func() error { return recorded(ctx, c) })

	// The TTL is envTTL, so a deleted violator stays recorded until it expires.
	time.Sleep(3 * time.Second)

	if err := recorded(ctx, c); err != nil {
		t.Fatalf("after the violation was deleted: %v", err)
	}
}

func TestNewNamespaceViolationIsRecorded(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	// Observe mode, so the violations can be created though the policy denies them. The policy and
	// binding go in first, with no namespaces, so every namespace below is new to the controller.
	fixture := violatingFixture(map[string]string{modeAnnotation: modeObserve})
	createAll(t, c, fixture[2:])

	// The exemption is Synced once a pass has run, and the informers are running by then.
	eventually(t, time.Minute, func() error {
		pe := &karv1alpha1.PolicyExemption{}
		if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
			return fmt.Errorf("get exemption: %w", err)
		}

		if !meta.IsStatusConditionTrue(pe.Status.Conditions, karv1alpha1.ConditionSynced) {
			return fmt.Errorf("%w: conditions %+v, want Synced True", errMismatch, pe.Status.Conditions)
		}

		return nil
	})

	// Each namespace is labelled when created, and its violation follows at once, so the object can
	// arrive before the controller has cached the Namespace.
	want := make([]string, 0, 20)

	for i := range 20 {
		ns := fmt.Sprintf("new-%d", i)
		want = append(want, ns)
		createAll(t, c, []client.Object{fixtureNS(ns), violation(ns)})
	}

	eventually(t, 30*time.Second, func() error { return recordedAll(ctx, c, want) })
}

func TestDeletedViolatorExpires(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("waits for a resync pass to expire the exemption")
	}

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce, ttlAnnotation: "1s"}))

	eventually(t, time.Minute, func() error { return recorded(ctx, c) })

	// Delete the ConfigMap, not fix it. Only the next pass notices the expiry.
	if err := c.Delete(ctx, violation("bad")); err != nil {
		t.Fatalf("delete violation: %v", err)
	}

	eventually(t, 150*time.Second, func() error {
		if recorded(ctx, c) == nil {
			return errStillRecorded
		}

		return derivedIs(ctx, c, nil)
	})
}

// configMapRestarts returns the informer restart counter of configmaps.
func configMapRestarts(t *testing.T) float64 {
	t.Helper()

	m := &dto.Metric{}
	if err := informerRestarts.WithLabelValues(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}.String()).Write(m); err != nil {
		t.Fatalf("read restart counter: %v", err)
	}

	return m.GetCounter().GetValue()
}

// TestRelabelRecordsExistingViolator reads the package-level informer restart counter, so it must not run in parallel.
//
//nolint:paralleltest // reads the global informer restart counter
func TestRelabelRecordsExistingViolator(t *testing.T) {
	c := startEnv(t)
	ctx := t.Context()

	late := &corev1.Namespace{Name: "late"}

	createAll(t, c, append(violatingFixture(map[string]string{modeAnnotation: modeEnforce}), late, violation("late")))

	eventually(t, time.Minute, func() error { return derivedIs(ctx, c, []string{"bad"}) })

	// late is not selected by the policy yet, so its violation is not evaluated.
	if err := c.Get(ctx, client.ObjectKey{Name: "late"}, late); err != nil {
		t.Fatalf("get namespace: %v", err)
	}

	if late.Labels == nil {
		late.Labels = map[string]string{}
	}

	late.Labels["kar-test"] = "fixture"

	before := configMapRestarts(t)

	if err := c.Update(ctx, late); err != nil {
		t.Fatalf("label namespace: %v", err)
	}

	eventually(t, 20*time.Second, func() error {
		if err := recordedAll(ctx, c, []string{"late"}); err != nil {
			return err
		}

		return derivedIs(ctx, c, []string{"bad", "late"})
	})

	if got := configMapRestarts(t); got != before {
		t.Fatalf("relabel restarted the informer: counter %v, was %v", got, before)
	}
}

// TestPolicyChangeReevaluates reads the package-level informer restart counter, so it must not run in parallel.
//
//nolint:paralleltest // reads the global informer restart counter
func TestPolicyChangeReevaluates(t *testing.T) {
	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, append(violatingFixture(map[string]string{modeAnnotation: modeEnforce}),
		fixtureNS("good"),
		&corev1.ConfigMap{Name: "app", Namespace: "good", Labels: map[string]string{"owner": "team"}},
	))

	eventually(t, time.Minute, func() error { return derivedIs(ctx, c, []string{"bad"}) })

	before := configMapRestarts(t)

	// A TTL change is not a spec change for the policy, so it must not restart the informer.
	b := &admissionregv1.ValidatingAdmissionPolicyBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, b); err != nil {
		t.Fatalf("get binding: %v", err)
	}

	b.Annotations[ttlAnnotation] = "100h"

	if err := c.Update(ctx, b); err != nil {
		t.Fatalf("update binding ttl: %v", err)
	}

	time.Sleep(5 * time.Second)

	if got := configMapRestarts(t); got != before {
		t.Fatalf("ttl change restarted the informer: counter %v, was %v", got, before)
	}

	// A policy spec change restarts the informer. good's compliant ConfigMap now violates, with no change to it.
	p := &admissionregv1.ValidatingAdmissionPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, p); err != nil {
		t.Fatalf("get policy: %v", err)
	}

	p.Spec.Validations[0].Expression = "has(object.metadata.labels) && 'tier' in object.metadata.labels"

	if err := c.Update(ctx, p); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	eventually(t, time.Minute, func() error { return derivedIs(ctx, c, []string{"bad", "good"}) })

	if got := configMapRestarts(t); got <= before {
		t.Fatalf("policy change did not restart the informer: counter %v, was %v", got, before)
	}
}

// widgetCRD is served at v1 (storage) and v1beta1 with one schema, so a rule can name either version.
func widgetCRD() *apiextensionsv1.CustomResourceDefinition {
	validation := &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: new(true)}}
	version := func(name string, storage bool) apiextensionsv1.CustomResourceDefinitionVersion {
		return apiextensionsv1.CustomResourceDefinitionVersion{Name: name, Served: true, Storage: storage, Schema: validation}
	}

	return &apiextensionsv1.CustomResourceDefinition{
		Name: "widgets.kar.test",
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group:    "kar.test",
			Scope:    apiextensionsv1.NamespaceScoped,
			Names:    apiextensionsv1.CustomResourceDefinitionNames{Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{version("v1", true), version("v1beta1", false)},
		},
	}
}

// TestPinnedVersionViolationIsExempted checks that a rule pinning a non-preferred version audits the objects
// the API server checks at that version. A violation stored at v1 must still exempt its namespace.
func TestPinnedVersionViolationIsExempted(t *testing.T) {
	t.Parallel()

	cfg := startAPIServer(t, nil)
	c := newClient(t, cfg)
	ctx := t.Context()

	// The CRD must be served before the controller's first discovery.
	createAll(t, c, []client.Object{widgetCRD()})

	eventually(t, time.Minute, func() error {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(ctx, client.ObjectKey{Name: "widgets.kar.test"}, crd); err != nil {
			return fmt.Errorf("get CRD: %w", err)
		}

		if !slices.ContainsFunc(crd.Status.Conditions, func(cond apiextensionsv1.CustomResourceDefinitionCondition) bool {
			return cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue
		}) {
			return errNotEstablished
		}

		return nil
	})

	widget := &unstructured.Unstructured{}
	widget.SetGroupVersionKind(schema.GroupVersionKind{Group: "kar.test", Version: "v1", Kind: "Widget"})
	widget.SetName("app")
	widget.SetNamespace("bad")

	createAll(t, c, []client.Object{
		fixtureNS("bad"),
		widget,
		&admissionregv1.ValidatingAdmissionPolicy{
			Name: sourceName,
			Spec: admissionregv1.ValidatingAdmissionPolicySpec{
				MatchConstraints: &admissionregv1.MatchResources{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kar-test": "fixture"}},
					ResourceRules: []admissionregv1.NamedRuleWithOperations{{
						Operations:  []admissionregv1.OperationType{admissionregv1.Create, admissionregv1.Update},
						APIGroups:   []string{"kar.test"},
						APIVersions: []string{"v1beta1"},
						Resources:   []string{"widgets"},
					}},
				},
				Validations: []admissionregv1.Validation{{Expression: "has(object.metadata.labels) && 'owner' in object.metadata.labels"}},
			},
		},
		&admissionregv1.ValidatingAdmissionPolicyBinding{
			Name:        sourceName,
			Annotations: map[string]string{modeAnnotation: modeEnforce},
			Spec: admissionregv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName:        sourceName,
				ValidationActions: []admissionregv1.ValidationAction{admissionregv1.Warn},
			},
		},
	})

	startController(t, cfg)

	eventually(t, 30*time.Second, func() error { return derivedIs(ctx, c, []string{"bad"}) })
}

func TestUnauditablePolicyRemovesDenyBinding(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce}))

	eventually(t, time.Minute, func() error { return derivedIs(ctx, c, []string{"bad"}) })

	p := &admissionregv1.ValidatingAdmissionPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, p); err != nil {
		t.Fatalf("get policy: %v", err)
	}

	p.Spec.Validations[0].Expression = "oldObject == null || " + p.Spec.Validations[0].Expression

	if err := c.Update(ctx, p); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	eventually(t, time.Minute, func() error {
		pe := &karv1alpha1.PolicyExemption{}
		if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
			return fmt.Errorf("get exemption: %w", err)
		}

		cond := meta.FindStatusCondition(pe.Status.Conditions, karv1alpha1.ConditionAuditable)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			return fmt.Errorf("%w: Auditable %v", errMismatch, cond)
		}

		return nil
	})

	eventually(t, time.Minute, func() error {
		err := c.Get(ctx, client.ObjectKey{Name: derivedName(sourceName)}, &admissionregv1.ValidatingAdmissionPolicyBinding{})
		if err == nil {
			return errStillDerived
		}

		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get derived binding: %w", err)
		}

		return nil
	})
}
