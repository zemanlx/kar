//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	karv1alpha1 "github.com/zemanlx/kar/api/v1alpha1"
)

const (
	leMetric     = "leader_election_master_status"
	leRestartKey = "kubectl.kubernetes.io/restartedAt"
	leHandoverBy = 5 * time.Second
)

var (
	errState       = errors.New("unexpected state")
	errLeaderCount = errors.New("not exactly one leader")
	errNotRunning  = errors.New("lease holder is not a running pod")
	errNotRolled   = errors.New("rollout not finished")
)

// TestLeaderElection runs its subtests in order against one enforce and one observe fixture. Each subtest kills or
// replaces kar pods, so none of them run in parallel.
//
//nolint:paralleltest // serial by design: the subtests kill kar pods
func TestLeaderElection(t *testing.T) {
	f := newFixture(t, "enforce", "", 1, 0)
	o := newFixture(t, "observe", "", 0, 1)

	var want []string

	eventually(t, 2*time.Minute, func() error {
		b, err := derived(t.Context(), f.Name)
		if err != nil {
			return err
		}

		if !slices.Equal(notIn(b), f.Violating) {
			return fmt.Errorf("%w: notIn = %v, want %v", errState, notIn(b), f.Violating)
		}

		if err := leExempts(t.Context(), f.Name, f.Violating[0]); err != nil {
			return err
		}

		want = notIn(b)

		return nil
	})

	t.Run("single leader", func(t *testing.T) {
		var lead corev1.Pod

		eventually(t, time.Minute, func() error {
			var err error

			lead, err = leSingleLeader(t.Context())

			return err
		})

		t.Logf("leader is %s", lead.Name)
	})

	stop := leWatch(t, f, want)

	t.Run("failover", func(t *testing.T) { leFailover(t, o) })
	t.Run("graceful handover", leGracefulHandover)
	t.Run("rollout restart", leRolloutRestart)

	stop()
}

// leFailover force-deletes the leader and checks that a new leader takes over and still reconciles.
func leFailover(t *testing.T, o fixture) {
	t.Helper()

	old, err := leader(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if err := kube.Delete(t.Context(), &old, client.GracePeriodSeconds(0)); err != nil {
		t.Fatalf("force delete leader %s: %v", old.Name, err)
	}

	eventually(t, 30*time.Second, func() error {
		cur, err := leader(t.Context())
		if err != nil {
			return err
		}

		if cur.Name == old.Name {
			return fmt.Errorf("%w: lease still held by deleted pod %s", errState, old.Name)
		}

		if cur.Status.Phase != corev1.PodRunning || cur.DeletionTimestamp != nil {
			return fmt.Errorf("%w: %s", errNotRunning, cur.Name)
		}

		return nil
	})

	eventually(t, time.Minute, func() error {
		_, err := leSingleLeader(t.Context())

		return err
	})

	app2 := deployment(o.Clean[0], nil)
	app2.Name = "app2"

	if err := kube.Create(t.Context(), app2); err != nil {
		t.Fatalf("create app2 in %s: %v", o.Clean[0], err)
	}

	eventually(t, time.Minute, func() error {
		return leExempts(t.Context(), o.Name, o.Clean[0])
	})
}

// leGracefulHandover deletes the leader normally and checks the Lease moves within leHandoverBy.
func leGracefulHandover(t *testing.T) {
	t.Helper()

	old, err := leader(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()

	if err := kube.Delete(t.Context(), &old); err != nil {
		t.Fatalf("delete leader %s: %v", old.Name, err)
	}

	for {
		if cur, err := leader(t.Context()); err == nil && cur.Name != old.Name {
			break
		}

		if time.Since(start) >= leHandoverBy {
			t.Fatalf("lease still held by %s %s after delete", old.Name, time.Since(start))
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Logf("handover from %s took %s", old.Name, time.Since(start))

	eventually(t, time.Minute, func() error {
		_, err := leSingleLeader(t.Context())

		return err
	})
}

// leRolloutRestart restarts the kar Deployment and checks the rollout finishes with a single leader.
func leRolloutRestart(t *testing.T) {
	t.Helper()

	var dep appsv1.Deployment
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: karName}, &dep); err != nil {
		t.Fatalf("get deployment %s: %v", karName, err)
	}

	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}

	dep.Spec.Template.Annotations[leRestartKey] = time.Now().Format(time.RFC3339)

	if err := kube.Patch(t.Context(), &dep, patch); err != nil {
		t.Fatalf("patch deployment %s: %v", karName, err)
	}

	eventually(t, 3*time.Minute, func() error {
		return leRolledOut(t.Context())
	})

	eventually(t, time.Minute, func() error {
		_, err := leSingleLeader(t.Context())

		return err
	})
}

// leSingleLeader returns the leader pod once exactly one running kar pod reports leader_election_master_status 1,
// the Lease holder is that pod, and replicas kar pods are running.
func leSingleLeader(ctx context.Context) (corev1.Pod, error) {
	pods, err := karPods(ctx)
	if err != nil {
		return corev1.Pod{}, err
	}

	if len(pods) != replicas {
		return corev1.Pod{}, fmt.Errorf("%w: %d running kar pods, want %d", errState, len(pods), replicas)
	}

	var leaders []string

	for _, p := range pods {
		text, err := metrics(ctx, p.Name)
		if err != nil {
			return corev1.Pod{}, err
		}

		v, err := metricValue(text, leMetric, fmt.Sprintf("name=%q", leaseName))
		if err != nil {
			return corev1.Pod{}, fmt.Errorf("pod %s: %w", p.Name, err)
		}

		if v == 1 {
			leaders = append(leaders, p.Name)
		}
	}

	if len(leaders) != 1 {
		return corev1.Pod{}, fmt.Errorf("%w: %v report leadership", errLeaderCount, leaders)
	}

	lead, err := leader(ctx)
	if err != nil {
		return corev1.Pod{}, err
	}

	if lead.Name != leaders[0] {
		return corev1.Pod{}, fmt.Errorf("%w: lease held by %s, but %s reports leadership", errState, lead.Name, leaders[0])
	}

	return lead, nil
}

// leRolledOut returns an error until the kar Deployment has finished rolling out replicas updated, ready pods, and
// exactly replicas kar pods are running.
func leRolledOut(ctx context.Context) error {
	var dep appsv1.Deployment
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ns, Name: karName}, &dep); err != nil {
		return fmt.Errorf("get deployment: %w", err)
	}

	s := dep.Status
	if s.ObservedGeneration < dep.Generation {
		return fmt.Errorf("%w: generation %d not observed", errNotRolled, dep.Generation)
	}

	if s.UpdatedReplicas != replicas || s.ReadyReplicas != replicas || s.Replicas != replicas {
		return fmt.Errorf("%w: updated %d, ready %d, total %d", errNotRolled, s.UpdatedReplicas, s.ReadyReplicas, s.Replicas)
	}

	pods, err := karPods(ctx)
	if err != nil {
		return err
	}

	if len(pods) != replicas {
		return fmt.Errorf("%w: %d running kar pods", errNotRolled, len(pods))
	}

	return nil
}

// leExempts returns an error unless the exemption named name lists namespace.
func leExempts(ctx context.Context, name, namespace string) error {
	pe, err := exemption(ctx, name)
	if err != nil {
		return err
	}

	if !leListed(pe, namespace) {
		return fmt.Errorf("%w: exemption %s does not list %s", errState, name, namespace)
	}

	return nil
}

// leListed reports whether pe's status lists namespace.
func leListed(pe *karv1alpha1.PolicyExemption, namespace string) bool {
	return slices.ContainsFunc(pe.Status.Namespaces, func(n karv1alpha1.ExemptNamespace) bool {
		return n.Name == namespace
	})
}

// leWatch checks the enforce fixture every pollInterval until the returned stop func is called. stop ends the watch,
// waits for it, and fails t with the first disruption seen, if any.
func leWatch(t *testing.T, f fixture, want []string) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	var first error

	go func() {
		defer close(done)

		tick := time.NewTicker(pollInterval)
		defer tick.Stop()

		for {
			if err := leDisruption(ctx, f, want); err != nil && first == nil {
				first = err
			}

			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	return func() {
		cancel()
		<-done

		if first != nil {
			t.Errorf("enforce policy disrupted: %v", first)
		}
	}
}

// leDisruption returns why the enforce fixture's derived binding or exemption is not as expected, or nil. Errors
// other than NotFound are transient and ignored.
func leDisruption(ctx context.Context, f fixture, want []string) error {
	b, err := derived(ctx, f.Name)

	switch {
	case err != nil && !apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !slices.Equal(notIn(b), want):
		return fmt.Errorf("%w: derived notIn = %v, want %v", errState, notIn(b), want)
	}

	pe, err := exemption(ctx, f.Name)

	switch {
	case err != nil && !apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !leListed(pe, f.Violating[0]):
		return fmt.Errorf("%w: exemption %s no longer lists %s", errState, f.Name, f.Violating[0])
	}

	return nil
}
