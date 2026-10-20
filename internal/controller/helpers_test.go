package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	karv1alpha1 "github.com/zemanlx/kar/api/v1alpha1"
)

// Outcomes of converge and settle.
const (
	outcomeConverged   = "converged"
	outcomeTooLarge    = "StateTooLarge"
	outcomeBudgetSpent = "budget"
)

// nsName is the i-th load namespace. Its 63 characters are the longest a namespace name may be.
func nsName(i int) string { return fmt.Sprintf("load-%058d", i) }

// names returns the first n load namespaces. They are already sorted, as the controller sorts them.
func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = nsName(i)
	}

	return out
}

// addNamespaces creates the violating namespaces [from, to), 32 at a time. A namespace is labeled
// only after its ConfigMap exists: once labeled, the enforce binding denies the ConfigMap.
func addNamespaces(t *testing.T, c client.Client, from, to int) {
	t.Helper()

	jobs := make(chan int)

	var wg sync.WaitGroup

	for range 32 {
		wg.Go(func() {
			for i := range jobs {
				ns := nsName(i)
				n := &corev1.Namespace{Name: ns}

				if err := c.Create(t.Context(), n); err != nil {
					t.Errorf("create namespace %s: %v", ns, err)

					continue
				}

				if err := c.Create(t.Context(), violation(ns)); err != nil {
					t.Errorf("create ConfigMap in %s: %v", ns, err)

					continue
				}

				n.Labels = fixtureNS(ns).Labels
				if err := c.Update(t.Context(), n); err != nil {
					t.Errorf("label namespace %s: %v", ns, err)
				}
			}
		})
	}

	for i := from; i < to; i++ {
		jobs <- i
	}

	close(jobs)
	wg.Wait()
}

// poke touches the source binding, so the controller runs a pass now. kar watches Namespaces, but a
// Namespace create triggers no pass, so without this a new namespace waits for the next resync (a
// minute). That wait would dominate the convergence time instead of the audit's cost.
func poke(t *testing.T, c client.Client, n int) {
	t.Helper()

	b := &admissionregv1.ValidatingAdmissionPolicyBinding{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: sourceName}, b); err != nil {
		t.Fatalf("get source binding: %v", err)
	}

	b.Annotations["load.kar.zeman.dev/step"] = strconv.Itoa(n)
	if err := c.Update(t.Context(), b); err != nil {
		t.Fatalf("touch source binding: %v", err)
	}
}

// converge polls until the exemption state and the derived binding both list want, the controller
// reports StateTooLarge, or budget passes. It returns the outcome and the time taken.
func converge(ctx context.Context, c client.Client, want []string, budget time.Duration) (string, time.Duration) {
	start := time.Now()
	outcome := outcomeBudgetSpent

	_ = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, budget, true, func(context.Context) (bool, error) {
		pe := &karv1alpha1.PolicyExemption{}
		if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
			return false, nil //nolint:nilerr // not created yet; keep polling
		}

		if cond := meta.FindStatusCondition(pe.Status.Conditions, karv1alpha1.ConditionSynced); cond != nil &&
			cond.Reason == karv1alpha1.ReasonStateTooLarge {
			outcome = outcomeTooLarge

			return true, nil
		}

		got := make([]string, 0, len(pe.Status.Namespaces))
		for _, e := range pe.Status.Namespaces {
			got = append(got, e.Name)
		}

		if slices.Equal(got, want) && derivedIs(ctx, c, want) == nil {
			outcome = outcomeConverged

			return true, nil
		}

		return false, nil
	})

	return outcome, time.Since(start)
}

// sizes returns the JSON size of the exemption status and of the derived binding spec. Missing objects are 0.
func sizes(ctx context.Context, c client.Client) (int, int, error) {
	var status, spec int

	pe := &karv1alpha1.PolicyExemption{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err == nil {
		b, err := json.Marshal(pe.Status)
		if err != nil {
			return 0, 0, fmt.Errorf("marshal status: %w", err)
		}

		status = len(b)
	}

	b := &admissionregv1.ValidatingAdmissionPolicyBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: derivedName(sourceName)}, b); err == nil {
		j, err := json.Marshal(b.Spec)
		if err != nil {
			return 0, 0, fmt.Errorf("marshal spec: %w", err)
		}

		spec = len(j)
	}

	return status, spec, nil
}

// heapMiB collects garbage and returns the heap in use, in MiB.
func heapMiB() float64 {
	runtime.GC()

	var ms runtime.MemStats

	runtime.ReadMemStats(&ms)

	return float64(ms.HeapInuse) / (1 << 20)
}

// createEach creates mk(i) for i in [from, to), 32 at a time.
func createEach(t *testing.T, c client.Client, from, to int, mk func(int) client.Object) {
	t.Helper()

	jobs := make(chan int)

	var wg sync.WaitGroup

	for range 32 {
		wg.Go(func() {
			for i := range jobs {
				if err := c.Create(t.Context(), mk(i)); err != nil {
					t.Errorf("create object %d: %v", i, err)
				}
			}
		})
	}

	for i := from; i < to; i++ {
		jobs <- i
	}

	close(jobs)
	wg.Wait()
}

// watchNS is the i-th of the 10 compliant namespaces that owned ConfigMaps are spread over.
func watchNS(i int) string { return fmt.Sprintf("watch-%02d", i) }

// watchNamespaces creates the 10 compliant namespaces.
func watchNamespaces(t *testing.T, c client.Client) {
	t.Helper()

	objs := make([]client.Object, 0, 10)
	for i := range 10 {
		objs = append(objs, fixtureNS(watchNS(i)))
	}

	createAll(t, c, objs)
}

// compliant is the i-th compliant ConfigMap: it carries the owner label the require-owner policy wants.
func compliant(i int) client.Object {
	return &corev1.ConfigMap{Name: fmt.Sprintf("obj-%d", i), Namespace: watchNS(i % 10), Labels: map[string]string{"owner": "team"}}
}

// settle polls check every 200ms until it returns nil or budget passes. It returns the outcome and the time taken.
func settle(ctx context.Context, budget time.Duration, check func(context.Context) error) (string, time.Duration) {
	start := time.Now()
	outcome := outcomeBudgetSpent

	if wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, budget, true, func(ctx context.Context) (bool, error) {
		return check(ctx) == nil, nil
	}) == nil {
		outcome = outcomeConverged
	}

	return outcome, time.Since(start)
}

// policyName is the name of the i-th source binding in TestLoadPolicies.
func policyName(i int) string { return fmt.Sprintf("require-owner-%03d", i) }

// policiesSettled returns an error unless each of the first n sources has an exemption listing want and a
// derived binding.
func policiesSettled(ctx context.Context, c client.Client, n int, want []string) error {
	for i := range n {
		name := policyName(i)

		pe := &karv1alpha1.PolicyExemption{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, pe); err != nil {
			return fmt.Errorf("get exemption %s: %w", name, err)
		}

		got := make([]string, 0, len(pe.Status.Namespaces))
		for _, e := range pe.Status.Namespaces {
			got = append(got, e.Name)
		}

		if !slices.Equal(got, want) {
			return fmt.Errorf("%w: %s lists %v, want %v", errMismatch, name, got, want)
		}

		if err := c.Get(ctx, client.ObjectKey{Name: derivedName(name)}, &admissionregv1.ValidatingAdmissionPolicyBinding{}); err != nil {
			return fmt.Errorf("get derived binding %s: %w", derivedName(name), err)
		}
	}

	return nil
}
