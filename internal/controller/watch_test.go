package controller

import (
	"context"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/zemanlx/kat/evaluator"
)

// blockingNamespaces is a live reader whose Get blocks until release is closed.
type blockingNamespaces struct {
	client.Reader

	entered chan struct{}
	release chan struct{}
}

func (b *blockingNamespaces) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	close(b.entered)
	<-b.release
	obj.SetName(key.Name)

	return nil
}

// denyPodsAuditor returns an auditor whose policy denies every CREATE of pods, compiled by r.
func denyPodsAuditor(t *testing.T, r *Reconciler, w watched) (*auditor, *admissionregv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()

	policy := &admissionregv1.ValidatingAdmissionPolicy{
		Name: "deny-all",
		Spec: admissionregv1.ValidatingAdmissionPolicySpec{
			MatchConstraints: matchRules(admissionregv1.Create),
			Validations:      []admissionregv1.Validation{{Expression: "false", Message: "denied"}},
		},
	}
	source := &admissionregv1.ValidatingAdmissionPolicyBinding{
		Name: "deny-pods",
		Spec: admissionregv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        "deny-all",
			ValidationActions: []admissionregv1.ValidationAction{admissionregv1.Deny},
		},
	}

	compiled, _, err := r.compile(policy, source)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	return &auditor{source: source, policy: policy, compiled: compiled, watched: []watched{w}}, source
}

func TestDenialDoesNotHoldAuditorLock(t *testing.T) {
	t.Parallel()

	ev, err := evaluator.New()
	if err != nil {
		t.Fatalf("create evaluator: %v", err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	r := &Reconciler{
		Client:    fake.NewClientBuilder().WithScheme(newScheme()).Build(), // no Namespaces: a cache miss
		apiReader: &blockingNamespaces{entered: entered, release: release},
		Evaluator: ev,
	}

	w := watched{GVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"}, GVK: corev1.SchemeGroupVersion.WithKind("Pod"), Namespaced: true}
	a, source := denyPodsAuditor(t, r, w)

	pod := &unstructured.Unstructured{}
	pod.SetAPIVersion("v1")
	pod.SetKind("Pod")
	pod.SetNamespace("team-a")
	pod.SetName("p1")

	type verdict struct {
		name, msg string
		denied    bool
	}

	done := make(chan verdict, 1)

	go func() {
		name, msg, denied := r.denial(t.Context(), a, w, pod)
		done <- verdict{name, msg, denied}
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("denial did not read the namespace")
	}

	if !a.mu.TryLock() {
		t.Errorf("denial holds a.mu while evaluating")
	} else {
		a.mu.Unlock()
	}

	close(release)

	got := <-done
	if got.name != source.Name || got.msg != "denied" || !got.denied {
		t.Errorf("denial() = %+v, want deny by %q with message %q", got, source.Name, "denied")
	}
}
