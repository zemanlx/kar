//go:build e2e

// Package e2e holds black-box tests for a kar install on a kind cluster. hack/e2e.sh installs the chart first.
package e2e

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	karv1alpha1 "github.com/zemanlx/kar/api/v1alpha1"
)

const (
	ns           = "kar-system"
	karName      = "kar" // Deployment, ServiceAccount, and the app.kubernetes.io/name label value
	leaseName    = "kar.zeman.dev"
	metricsPort  = 8080
	pollInterval = 500 * time.Millisecond
	maxNameLen   = 40
	labelKey     = "kar-e2e" // namespace label that scopes a fixture's policy
	modeKey      = "kar.zeman.dev/mode"
	ttlKey       = "kar.zeman.dev/ttl"
	pauseImage   = "registry.k8s.io/pause:3.10"
)

var (
	errNoHolder      = errors.New("lease has no holderIdentity")
	errMetricMissing = errors.New("metric not found")
)

// kube and clientset are built once in TestMain and shared by every test in the package.
//
//nolint:gochecknoglobals // set once in TestMain, read-only afterwards
var (
	kube      client.Client
	clientset kubernetes.Interface
)

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}

	code := m.Run()
	if code == 0 {
		// Runs after every test, so it sees the memory the whole suite left behind.
		if err := checkMemory(); err != nil {
			fmt.Fprintln(os.Stderr, "memory headroom:", err)

			code = 1
		}
	}

	os.Exit(code)
}

// setup builds the controller-runtime and clientset clients from KUBECONFIG.
func setup() error {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := errors.Join(clientgoscheme.AddToScheme(scheme), karv1alpha1.AddToScheme(scheme)); err != nil {
		return fmt.Errorf("scheme: %w", err)
	}

	if kube, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		return fmt.Errorf("client: %w", err)
	}

	if clientset, err = kubernetes.NewForConfig(cfg); err != nil {
		return fmt.Errorf("clientset: %w", err)
	}

	return nil
}

// eventually polls f every 500ms until it returns nil, then fails t with the last error if timeout passes first.
func eventually(t *testing.T, timeout time.Duration, f func() error) {
	t.Helper()

	var last error

	err := wait.PollUntilContextTimeout(t.Context(), pollInterval, timeout, true, func(context.Context) (bool, error) {
		last = f()

		return last == nil, nil
	})
	if err != nil {
		t.Fatalf("condition not met within %s: %v", timeout, last)
	}
}

// karPods returns the kar pods that are running and not being deleted.
func karPods(ctx context.Context) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := kube.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{"app.kubernetes.io/name": karName}); err != nil {
		return nil, fmt.Errorf("list kar pods: %w", err)
	}

	var running []corev1.Pod

	for _, p := range list.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}

	return running, nil
}

// leader returns the pod holding the kar.zeman.dev Lease. The lease holderIdentity is "<pod>_<uuid>".
func leader(ctx context.Context) (corev1.Pod, error) {
	var lease coordinationv1.Lease
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ns, Name: leaseName}, &lease); err != nil {
		return corev1.Pod{}, fmt.Errorf("get lease: %w", err)
	}

	if lease.Spec.HolderIdentity == nil {
		return corev1.Pod{}, errNoHolder
	}

	name, _, _ := strings.Cut(*lease.Spec.HolderIdentity, "_")

	var pod corev1.Pod
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pod); err != nil {
		return corev1.Pod{}, fmt.Errorf("get leader pod %s: %w", name, err)
	}

	return pod, nil
}

// metrics returns the Prometheus text exposition served by pod, read through the API server proxy.
func metrics(ctx context.Context, pod string) (string, error) {
	b, err := clientset.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/namespaces", ns, "pods", fmt.Sprintf("%s:%d", pod, metricsPort), "proxy", "metrics").
		DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("scrape %s: %w", pod, err)
	}

	return string(b), nil
}

// metricValue returns the value of the first sample named name whose line contains every label fragment.
func metricValue(text, name string, labels ...string) (float64, error) {
	for line := range strings.Lines(text) {
		if !strings.HasPrefix(line, name+" ") && !strings.HasPrefix(line, name+"{") {
			continue
		}

		matches := true

		for _, l := range labels {
			if !strings.Contains(line, l) {
				matches = false

				break
			}
		}

		if !matches {
			continue
		}

		fields := strings.Fields(line)

		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("parse %q: %w", line, err)
		}

		return v, nil
	}

	return 0, fmt.Errorf("%w: %s %v", errMetricMissing, name, labels)
}

// uniq returns a DNS-safe name for this test: its lowercased name, truncated, plus a random 5-character suffix.
func uniq(t *testing.T) string {
	t.Helper()

	name := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}

		return '-'
	}, strings.ToLower(t.Name()))
	name = strings.Trim(name[:min(len(name), maxNameLen)], "-")

	return name + "-" + strings.ToLower(rand.Text()[:5])
}

// fixture is a ValidatingAdmissionPolicy and a Warn binding, both named Name, applied to namespaces labelled kar-e2e=Name.
type fixture struct {
	Name      string
	Violating []string // namespaces whose Deployment has no owner label
	Clean     []string // namespaces whose Deployment has owner: e2e
}

// newFixture creates the namespaces and Deployments, then the policy and binding, and registers cleanup for all of
// them plus the exemption and derived binding kar writes. mode and ttl become the binding's annotations; an empty
// ttl is left off.
func newFixture(t *testing.T, mode, ttl string, violating, clean int) fixture {
	t.Helper()

	f := fixture{Name: uniq(t)}

	objs := make([]client.Object, 0, 2*(violating+clean)+2)

	for i := range violating {
		name := fmt.Sprintf("%s-v%d", f.Name, i)
		f.Violating = append(f.Violating, name)
		objs = append(objs, labelledNamespace(f.Name, name), deployment(name, nil))
	}

	for i := range clean {
		name := fmt.Sprintf("%s-c%d", f.Name, i)
		f.Clean = append(f.Clean, name)
		objs = append(objs, labelledNamespace(f.Name, name), deployment(name, map[string]string{"owner": "e2e"}))
	}

	objs = append(objs, policy(f.Name), binding(f.Name, mode, ttl))

	t.Cleanup(func() { cleanupFixture(t, f) })

	for _, obj := range objs {
		if err := kube.Create(t.Context(), obj); err != nil {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
	}

	return f
}

func labelledNamespace(fixtureName, name string) *corev1.Namespace {
	return &corev1.Namespace{Name: name, Labels: map[string]string{labelKey: fixtureName}}
}

// deployment is a Deployment with no pods, since kar audits Deployment objects, not what they run.
func deployment(namespace string, labels map[string]string) *appsv1.Deployment {
	app := map[string]string{"app": "pause"}

	return &appsv1.Deployment{
		Name: "app", Namespace: namespace, Labels: labels,
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32), // 0
			Selector: &metav1.LabelSelector{MatchLabels: app},
			Template: corev1.PodTemplateSpec{
				Labels: app,
				Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "pause", Image: pauseImage}}},
			},
		},
	}
}

func policy(name string) *admissionregv1.ValidatingAdmissionPolicy {
	return &admissionregv1.ValidatingAdmissionPolicy{
		Name: name,
		Spec: admissionregv1.ValidatingAdmissionPolicySpec{
			MatchConstraints: &admissionregv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelKey: name}},
				ResourceRules: []admissionregv1.NamedRuleWithOperations{{
					Operations:  []admissionregv1.OperationType{admissionregv1.Create, admissionregv1.Update},
					APIGroups:   []string{"apps"},
					APIVersions: []string{"v1"},
					Resources:   []string{"deployments"},
				}},
			},
			Validations: []admissionregv1.Validation{{
				Expression: "has(object.metadata.labels) && 'owner' in object.metadata.labels",
				Message:    "deployments need an owner label",
			}},
		},
	}
}

func binding(name, mode, ttl string) *admissionregv1.ValidatingAdmissionPolicyBinding {
	annotations := map[string]string{modeKey: mode}
	if ttl != "" {
		annotations[ttlKey] = ttl
	}

	return &admissionregv1.ValidatingAdmissionPolicyBinding{
		Name: name, Annotations: annotations,
		Spec: admissionregv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			ValidationActions: []admissionregv1.ValidationAction{admissionregv1.Warn},
		},
	}
}

// cleanupFixture deletes the fixture and what kar wrote for it. It runs after t's context is cancelled, so it
// uses its own. The source binding goes first so kar stops re-deriving the Deny binding.
func cleanupFixture(t *testing.T, f fixture) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	objs := make([]client.Object, 0, 4+len(f.Violating)+len(f.Clean))
	objs = append(objs,
		&admissionregv1.ValidatingAdmissionPolicyBinding{Name: f.Name},
		&admissionregv1.ValidatingAdmissionPolicyBinding{Name: derivedName(f.Name)},
		&karv1alpha1.PolicyExemption{Name: f.Name},
		&admissionregv1.ValidatingAdmissionPolicy{Name: f.Name},
	)

	for _, name := range append(f.Violating, f.Clean...) {
		objs = append(objs, &corev1.Namespace{Name: name})
	}

	for _, obj := range objs {
		if err := kube.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			t.Errorf("delete %T %s: %v", obj, obj.GetName(), err)
		}
	}
}

// derivedName is the Deny binding kar writes for a source binding in enforce mode.
func derivedName(source string) string {
	return source + "-enforce"
}

// exemption returns the PolicyExemption named name.
func exemption(ctx context.Context, name string) (*karv1alpha1.PolicyExemption, error) {
	var pe karv1alpha1.PolicyExemption
	if err := kube.Get(ctx, client.ObjectKey{Name: name}, &pe); err != nil {
		return nil, fmt.Errorf("get exemption %s: %w", name, err)
	}

	return &pe, nil
}

// derived returns the Deny binding kar derived from the source binding named source.
func derived(ctx context.Context, source string) (*admissionregv1.ValidatingAdmissionPolicyBinding, error) {
	var b admissionregv1.ValidatingAdmissionPolicyBinding
	if err := kube.Get(ctx, client.ObjectKey{Name: derivedName(source)}, &b); err != nil {
		return nil, fmt.Errorf("get derived binding: %w", err)
	}

	return &b, nil
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
