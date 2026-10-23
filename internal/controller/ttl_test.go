package controller

import (
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestShortenedTTLExpiresExemption(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("waits for a resync pass to expire the exemption")
	}

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce}))

	eventually(t, time.Minute, func() error { return recorded(ctx, c) })

	// Fixed before the TTL shrinks, so the exemption can only become due through the shorter TTL.
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "bad", Name: "app"}, cm); err != nil {
		t.Fatalf("get configmap: %v", err)
	}

	cm.Labels = map[string]string{"owner": "team"}
	if err := c.Update(ctx, cm); err != nil {
		t.Fatalf("label configmap: %v", err)
	}

	b := &admissionregv1.ValidatingAdmissionPolicyBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, b); err != nil {
		t.Fatalf("get binding: %v", err)
	}

	b.Annotations[ttlAnnotation] = "1s"

	if err := c.Update(ctx, b); err != nil {
		t.Fatalf("shorten ttl: %v", err)
	}

	eventually(t, 150*time.Second, func() error {
		if recorded(ctx, c) == nil {
			return errStillRecorded
		}

		return derivedIs(ctx, c, nil)
	})
}
