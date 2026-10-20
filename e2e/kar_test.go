//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	replicas     = 3
	saUser       = "system:serviceaccount:kar-system:kar"
	runAsUser    = int64(65532)
	mib          = 1 << 20
	karContainer = "kar"
	memoryCap    = 0.5 // fraction of the memory limit the leader may reach
)

var (
	errNotReady   = errors.New("deployment not ready")
	errNodes      = errors.New("kar pods not on distinct nodes")
	errNotTrue    = errors.New("condition not True")
	errMemory     = errors.New("leader over memory cap")
	errRestarted  = errors.New("kar container restarted or OOMKilled")
	errNoLimit    = errors.New("no memory limit on kar container")
	errNoLeaderUp = errors.New("leader is not among running kar pods")
)

type sarCase struct {
	name    string
	attrs   authorizationv1.ResourceAttributes
	allowed bool
}

func TestInstalledAndHealthy(t *testing.T) {
	t.Parallel()

	eventually(t, 2*time.Minute, func() error { return rolledOut(t.Context()) })

	if err := crdEstablished(t.Context(), "policyexemptions.kar.zeman.dev"); err != nil {
		t.Fatal(err)
	}

	lead, err := leader(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	text, err := metrics(t.Context(), lead.Name)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"controller_runtime_", "leader_election_master_status"} {
		if !strings.Contains(text, want) {
			t.Errorf("leader %s metrics lack %s", lead.Name, want)
		}
	}
}

// rolledOut returns an error until the kar Deployment has all replicas ready and its pods run on distinct nodes.
func rolledOut(ctx context.Context) error {
	var dep appsv1.Deployment
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ns, Name: karName}, &dep); err != nil {
		return fmt.Errorf("get deployment: %w", err)
	}

	if dep.Status.ReadyReplicas != replicas {
		return fmt.Errorf("%w: %d ready, want %d", errNotReady, dep.Status.ReadyReplicas, replicas)
	}

	pods, err := karPods(ctx)
	if err != nil {
		return err
	}

	nodes := make(map[string]bool, len(pods))
	for _, p := range pods {
		nodes[p.Spec.NodeName] = true
	}

	if len(pods) != replicas || len(nodes) != replicas {
		return fmt.Errorf("%w: %d pods on %d nodes", errNodes, len(pods), len(nodes))
	}

	return nil
}

// crdEstablished returns an error unless the named CRD has condition Established=True.
func crdEstablished(ctx context.Context, name string) error {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})

	if err := kube.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
		return fmt.Errorf("get crd %s: %w", name, err)
	}

	conds, _, err := unstructured.NestedSlice(crd.Object, "status", "conditions")
	if err != nil {
		return fmt.Errorf("read conditions of %s: %w", name, err)
	}

	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "Established" && m["status"] == "True" {
			return nil
		}
	}

	return fmt.Errorf("%w: %s Established", errNotTrue, name)
}

func TestHardening(t *testing.T) {
	t.Parallel()

	var nsObj corev1.Namespace
	if err := kube.Get(t.Context(), client.ObjectKey{Name: ns}, &nsObj); err != nil {
		t.Fatalf("get namespace %s: %v", ns, err)
	}

	if got := nsObj.Labels["pod-security.kubernetes.io/enforce"]; got != "restricted" {
		t.Errorf("pod-security enforce label = %q, want restricted", got)
	}

	pods, err := karPods(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(pods) == 0 {
		t.Fatal("no running kar pods")
	}

	for _, p := range pods {
		for _, failure := range hardeningFailures(p) {
			t.Errorf("pod %s: %s", p.Name, failure)
		}
	}

	cases := []sarCase{
		{"get own lease", leaseAccess(ns, "get", leaseName), true},
		{"update own lease", leaseAccess(ns, "update", leaseName), true},
		{"create leases", leaseAccess(ns, "create", ""), true},
		// No "get other lease" case: auditing needs cluster-wide read of every resource.
		{"update other lease", leaseAccess(ns, "update", "other"), false},
		{"create lease in default", leaseAccess("default", "create", ""), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sar := &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{User: saUser, ResourceAttributes: &tc.attrs},
			}
			if err := kube.Create(t.Context(), sar); err != nil {
				t.Fatalf("create SubjectAccessReview: %v", err)
			}

			if sar.Status.Allowed != tc.allowed {
				t.Errorf("allowed = %t, want %t: %s", sar.Status.Allowed, tc.allowed, sar.Status.Reason)
			}
		})
	}
}

// leaseAccess returns the access attributes for verb on the named lease in namespace; an empty name matches any lease.
func leaseAccess(namespace, verb, name string) authorizationv1.ResourceAttributes {
	return authorizationv1.ResourceAttributes{
		Namespace: namespace, Verb: verb, Group: "coordination.k8s.io", Resource: "leases", Name: name,
	}
}

// hardeningFailures lists the restricted-profile settings of pod that do not hold.
func hardeningFailures(p corev1.Pod) []string {
	var failed []string

	fail := func(ok bool, what string) {
		if !ok {
			failed = append(failed, what)
		}
	}

	psc := p.Spec.SecurityContext
	if psc == nil {
		psc = &corev1.PodSecurityContext{}
	}

	fail(is(psc.RunAsNonRoot, true), "runAsNonRoot")
	fail(is(psc.RunAsUser, runAsUser), "runAsUser 65532")
	fail(psc.SeccompProfile != nil && psc.SeccompProfile.Type == corev1.SeccompProfileTypeRuntimeDefault, "seccompProfile RuntimeDefault")
	fail(is(p.Spec.AutomountServiceAccountToken, false), "automountServiceAccountToken false")
	fail(is(p.Spec.EnableServiceLinks, false), "enableServiceLinks false")
	fail(!p.Spec.HostNetwork && !p.Spec.HostPID, "no hostNetwork or hostPID")

	for _, c := range p.Spec.Containers {
		csc := c.SecurityContext
		if csc == nil {
			csc = &corev1.SecurityContext{}
		}

		fail(is(csc.AllowPrivilegeEscalation, false), c.Name+" allowPrivilegeEscalation false")
		fail(is(csc.ReadOnlyRootFilesystem, true), c.Name+" readOnlyRootFilesystem true")
		fail(csc.Capabilities != nil && slices.Equal(csc.Capabilities.Drop, []corev1.Capability{"ALL"}), c.Name+" drops ALL capabilities")
	}

	return failed
}

// is reports whether p is set and equal to want.
func is[T comparable](p *T, want T) bool {
	return p != nil && *p == want
}

// checkMemory runs after the suite. It logs every kar pod's resident memory, and fails if the leader uses
// memoryCap of its memory limit or more, or if any kar container restarted or was OOMKilled.
func checkMemory() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	lead, err := leader(ctx)
	if err != nil {
		return err
	}

	pods, err := karPods(ctx)
	if err != nil {
		return err
	}

	rss := make(map[string]float64, len(pods))

	for _, p := range pods {
		if err := restartFailure(p); err != nil {
			return err
		}

		text, err := metrics(ctx, p.Name)
		if err != nil {
			return err
		}

		v, err := metricValue(text, "process_resident_memory_bytes")
		if err != nil {
			return fmt.Errorf("pod %s: %w", p.Name, err)
		}

		rss[p.Name] = v

		fmt.Fprintf(os.Stderr, "kar pod %s: resident %.1f MiB\n", p.Name, v/mib)
	}

	leaderRSS, ok := rss[lead.Name]
	if !ok {
		return fmt.Errorf("%w: %s", errNoLeaderUp, lead.Name)
	}

	limit, err := memoryLimit(lead)
	if err != nil {
		return err
	}

	if leaderRSS >= limit*memoryCap {
		return fmt.Errorf("%w: leader %s at %.1f MiB, limit %.0f MiB", errMemory, lead.Name, leaderRSS/mib, limit/mib)
	}

	return nil
}

// restartFailure returns an error if any container of p restarted or was OOMKilled.
func restartFailure(p corev1.Pod) error {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.RestartCount > 0 {
			return fmt.Errorf("%w: pod %s container %s restarted %d times", errRestarted, p.Name, cs.Name, cs.RestartCount)
		}

		if term := cs.LastTerminationState.Terminated; term != nil && term.Reason == "OOMKilled" {
			return fmt.Errorf("%w: pod %s container %s", errRestarted, p.Name, cs.Name)
		}
	}

	return nil
}

// memoryLimit returns the bytes in the kar container's memory limit.
func memoryLimit(p corev1.Pod) (float64, error) {
	for _, c := range p.Spec.Containers {
		if c.Name == karContainer {
			if q := c.Resources.Limits.Memory(); !q.IsZero() {
				return float64(q.Value()), nil
			}
		}
	}

	return 0, fmt.Errorf("%w: pod %s", errNoLimit, p.Name)
}
