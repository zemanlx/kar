package controller

import (
	"context"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Per-unit budgets, measured with the helpers the load ramps use. Each is the most a unit may cost. The byte
// sizes are deterministic (148.4 and 67.2 measured), so their budgets are tight; heap varies run to run, so
// its budgets are about 2x the highest measured.
const (
	regressionBudget         = time.Minute
	regressionNamespaces     = 200
	regressionObjects        = 20000
	regressionConfigMaps     = 1000
	regressionSourceCount    = 10
	regressionBindings       = 20
	statusBytesPerNamespace  = 160
	bindingBytesPerNamespace = 75
	heapKiBPerObject         = 1.5
	heapMiBPerBinding        = 0.6
)

// TestRegressionStateBytesPerNamespace guards the bytes kar writes per exempt namespace: the exemption status
// and the derived binding's NotIn list both grow with the namespaces they exempt.
func TestRegressionStateBytesPerNamespace(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce})[2:])
	addNamespaces(t, c, 0, regressionNamespaces)
	poke(t, c, regressionNamespaces)

	outcome, took := converge(ctx, c, names(regressionNamespaces), regressionBudget)
	if outcome != outcomeConverged {
		t.Fatalf("%d namespaces: %s after %s", regressionNamespaces, outcome, took)
	}

	status, binding, err := sizes(ctx, c)
	if err != nil {
		t.Fatalf("size state: %v", err)
	}

	statusPer := float64(status) / regressionNamespaces
	bindingPer := float64(binding) / regressionNamespaces
	t.Logf("status %.1f bytes per namespace (budget %d), binding %.1f (budget %d), converged in %s",
		statusPer, statusBytesPerNamespace, bindingPer, bindingBytesPerNamespace, took)

	if statusPer > statusBytesPerNamespace {
		t.Errorf("status is %.1f bytes per namespace, budget %d", statusPer, statusBytesPerNamespace)
	}

	if bindingPer > bindingBytesPerNamespace {
		t.Errorf("binding is %.1f bytes per namespace, budget %d", bindingPer, bindingBytesPerNamespace)
	}
}

// TestRegressionHeapPerObject guards the heap the controller keeps per watched ConfigMap. The informer caches
// every ConfigMap in the cluster, so this cost scales with the cluster, not with kar's own state.
//
//nolint:paralleltest // measures the process heap
func TestRegressionHeapPerObject(t *testing.T) {
	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce})[2:])
	watchNamespaces(t, c)
	addNamespaces(t, c, 0, 1)
	poke(t, c, 1)

	if outcome, took := converge(ctx, c, names(1), regressionBudget); outcome != outcomeConverged {
		t.Fatalf("1 namespace: %s after %s", outcome, took)
	}

	before := heapMiB()

	createEach(t, c, 0, regressionObjects, compliant)
	addNamespaces(t, c, 1, 2)
	poke(t, c, 2)

	if outcome, took := converge(ctx, c, names(2), regressionBudget); outcome != outcomeConverged {
		t.Fatalf("2 namespaces after %d objects: %s after %s", regressionObjects, outcome, took)
	}

	after := heapMiB()
	perObject := (after - before) * (1 << 10) / regressionObjects
	t.Logf("heap %.1f MiB before and %.1f MiB after %d objects: %.3f KiB per object (budget %.1f)",
		before, after, regressionObjects, perObject, heapKiBPerObject)

	if perObject > heapKiBPerObject {
		t.Errorf("heap is %.3f KiB per object, budget %.1f", perObject, heapKiBPerObject)
	}
}

// TestRegressionHeapPerBinding guards the heap the controller keeps per source binding, with the objects fixed
// as in TestLoadPolicies. One binding is settled first, so the baseline already has the informers warm.
//
//nolint:paralleltest // measures the process heap
func TestRegressionHeapPerBinding(t *testing.T) {
	c := startEnv(t)
	ctx := t.Context()

	base := violatingFixture(map[string]string{modeAnnotation: modeEnforce})
	policy, okPolicy := base[2].(*admissionregv1.ValidatingAdmissionPolicy)

	binding, okBinding := base[3].(*admissionregv1.ValidatingAdmissionPolicyBinding)
	if !okPolicy || !okBinding {
		t.Fatal("violatingFixture no longer has a policy and binding at 2 and 3")
	}

	addNamespaces(t, c, 0, regressionSourceCount)
	watchNamespaces(t, c)
	createEach(t, c, 0, regressionConfigMaps, compliant)

	want := names(regressionSourceCount)
	addSources := func(from, to int) {
		for i := from; i < to; i++ {
			p, b := policy.DeepCopy(), binding.DeepCopy()
			p.Name, b.Name, b.Spec.PolicyName = policyName(i), policyName(i), policyName(i)
			createAll(t, c, []client.Object{p, b})
		}
	}
	settled := func(n int) func(context.Context) error {
		return func(ctx context.Context) error { return policiesSettled(ctx, c, n, want) }
	}

	addSources(0, 1)

	if outcome, took := settle(ctx, regressionBudget, settled(1)); outcome != outcomeConverged {
		t.Fatalf("first binding: %s after %s", outcome, took)
	}

	before := heapMiB()

	addSources(1, 1+regressionBindings)

	if outcome, took := settle(ctx, regressionBudget, settled(1+regressionBindings)); outcome != outcomeConverged {
		t.Fatalf("%d bindings: %s after %s", 1+regressionBindings, outcome, took)
	}

	after := heapMiB()
	perBinding := (after - before) / regressionBindings
	t.Logf("heap %.1f MiB before and %.1f MiB after %d bindings: %.3f MiB per binding (budget %.1f)",
		before, after, regressionBindings, perBinding, heapMiBPerBinding)

	if perBinding > heapMiBPerBinding {
		t.Errorf("heap is %.3f MiB per binding, budget %.1f", perBinding, heapMiBPerBinding)
	}
}
