package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newNamespaceReconciler returns a Reconciler whose cache holds no Namespaces, as when a new namespace's
// objects arrive before the Namespace informer has synced, and whose live reader holds live.
func newNamespaceReconciler(live ...client.Object) *Reconciler {
	return &Reconciler{
		Client:    fake.NewClientBuilder().WithScheme(newScheme()).Build(),
		apiReader: fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(live...).Build(),
	}
}

func TestNamespaceExistsMissingFromCache(t *testing.T) {
	t.Parallel()

	r := newNamespaceReconciler(&corev1.Namespace{Name: "fresh"})

	tests := []struct {
		name string
		ns   string
		want bool
	}{
		{name: "new namespace known only to the live reader", ns: "fresh", want: true},
		{name: "namespace known to neither", ns: "gone", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := r.namespaceExists(t.Context(), tt.ns); got != tt.want {
				t.Errorf("namespaceExists(%q) = %v, want %v", tt.ns, got, tt.want)
			}
		})
	}
}
