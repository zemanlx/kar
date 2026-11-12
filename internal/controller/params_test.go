package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	karv1alpha1 "github.com/zemanlx/kar/api/v1alpha1"
)

// paramLabel marks a params ConfigMap. The policies do not match it, so a params object in a fixture
// namespace is not audited itself.
const paramLabel = "kar-test/params"

// tierMatches denies a ConfigMap whose tier label is not the params' tier.
const tierMatches = "has(object.metadata.labels) && 'tier' in object.metadata.labels && object.metadata.labels.tier == params.data.tier"

// configMapParams is the params kind of the policies that read ConfigMaps.
func configMapParams() *admissionregv1.ParamKind {
	return &admissionregv1.ParamKind{APIVersion: "v1", Kind: "ConfigMap"}
}

// paramsPolicy is the require-owner policy reading params of kind pk, with expression as its validation.
// It audits the ConfigMaps of fixture namespaces that are not params.
func paramsPolicy(pk *admissionregv1.ParamKind, expression string) *admissionregv1.ValidatingAdmissionPolicy {
	return &admissionregv1.ValidatingAdmissionPolicy{
		Name: sourceName,
		Spec: admissionregv1.ValidatingAdmissionPolicySpec{
			ParamKind: pk,
			MatchConstraints: &admissionregv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kar-test": "fixture"}},
				ObjectSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: paramLabel, Operator: metav1.LabelSelectorOpDoesNotExist},
				}},
				ResourceRules: []admissionregv1.NamedRuleWithOperations{{
					Operations:  []admissionregv1.OperationType{admissionregv1.Create, admissionregv1.Update},
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"configmaps"},
				}},
			},
			Validations: []admissionregv1.Validation{{Expression: expression}},
		},
	}
}

// paramsBinding is the observing source binding of the require-owner policy, with ref as its paramRef.
func paramsBinding(ref *admissionregv1.ParamRef) *admissionregv1.ValidatingAdmissionPolicyBinding {
	return &admissionregv1.ValidatingAdmissionPolicyBinding{
		Name:        sourceName,
		Annotations: map[string]string{modeAnnotation: modeObserve},
		Spec: admissionregv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        sourceName,
			ParamRef:          ref,
			ValidationActions: []admissionregv1.ValidationAction{admissionregv1.Warn},
		},
	}
}

// tierParam is a params ConfigMap named name in ns, with data.
func tierParam(ns, name string, labels, data map[string]string) *corev1.ConfigMap {
	l := map[string]string{paramLabel: "true"}
	maps.Copy(l, labels)

	return &corev1.ConfigMap{Name: name, Namespace: ns, Labels: l, Data: data}
}

// tiered is the audited ConfigMap of ns, labeled with tier unless tier is empty.
func tiered(ns, tier string) *corev1.ConfigMap {
	cm := violation(ns)
	if tier != "" {
		cm.Labels = map[string]string{"tier": tier}
	}

	return cm
}

// recordedExactly returns an error unless the audit has synced and the PolicyExemption lists exactly want,
// in order. Synced means every watched object has been evaluated, so a namespace missing from want is one
// that does not violate.
func recordedExactly(ctx context.Context, c client.Client, want []string) error {
	pe := &karv1alpha1.PolicyExemption{}
	if err := c.Get(ctx, client.ObjectKey{Name: sourceName}, pe); err != nil {
		return fmt.Errorf("get exemption: %w", err)
	}

	if !meta.IsStatusConditionTrue(pe.Status.Conditions, karv1alpha1.ConditionSynced) {
		return fmt.Errorf("%w: conditions %+v, want Synced True", errMismatch, pe.Status.Conditions)
	}

	got := make([]string, 0, len(pe.Status.Namespaces))
	for _, e := range pe.Status.Namespaces {
		got = append(got, e.Name)
	}

	if !slices.Equal(got, want) {
		return fmt.Errorf("%w: namespaces %v, want %v", errMismatch, got, want)
	}

	return nil
}

// TestParamRefNamespace checks that a paramRef with a namespace reads the params there, not in the
// audited object's namespace.
func TestParamRefNamespace(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, []client.Object{
		&corev1.Namespace{Name: "params"},
		tierParam("params", "tier", nil, map[string]string{"tier": "gold"}),
		fixtureNS("gold"), tiered("gold", "gold"),
		fixtureNS("silver"), tiered("silver", "silver"),
		tierParam("silver", "tier", nil, map[string]string{"tier": "silver"}), // a decoy paramRef.namespace overrides
		fixtureNS("unlabeled"), tiered("unlabeled", ""),
		paramsPolicy(configMapParams(), tierMatches),
		paramsBinding(&admissionregv1.ParamRef{Name: "tier", Namespace: "params", ParameterNotFoundAction: new(admissionregv1.DenyAction)}),
	})

	eventually(t, time.Minute, func() error { return recordedExactly(ctx, c, []string{"silver", "unlabeled"}) })
}

// TestParamRefObjectNamespace checks that a paramRef without a namespace reads the params in each audited
// object's namespace.
func TestParamRefObjectNamespace(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, []client.Object{
		fixtureNS("a"), tiered("a", "gold"), tierParam("a", "tier", nil, map[string]string{"tier": "gold"}),
		fixtureNS("b"), tiered("b", "silver"), tierParam("b", "tier", nil, map[string]string{"tier": "gold"}),
		fixtureNS("c"), tiered("c", "silver"), tierParam("c", "tier", nil, map[string]string{"tier": "silver"}),
		paramsPolicy(configMapParams(), tierMatches),
		paramsBinding(&admissionregv1.ParamRef{Name: "tier", ParameterNotFoundAction: new(admissionregv1.DenyAction)}),
	})

	eventually(t, time.Minute, func() error { return recordedExactly(ctx, c, []string{"b"}) })
}

// TestParamRefSelector checks that a paramRef selector evaluates the policy against each selected params
// object and denies when any of them denies, as the API server does. Unselected params are not read.
func TestParamRefSelector(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	selected := map[string]string{"kar-test/forbid": "true"}

	createAll(t, c, []client.Object{
		&corev1.Namespace{Name: "params"},
		tierParam("params", "no-bronze", selected, map[string]string{"forbidden": "bronze"}),
		tierParam("params", "no-copper", selected, map[string]string{"forbidden": "copper"}),
		tierParam("params", "no-gold", nil, map[string]string{"forbidden": "gold"}),
		fixtureNS("bronze"), tiered("bronze", "bronze"),
		fixtureNS("copper"), tiered("copper", "copper"),
		fixtureNS("gold"), tiered("gold", "gold"),
		fixtureNS("unlabeled"), tiered("unlabeled", ""),
		paramsPolicy(configMapParams(), "!(has(object.metadata.labels) && 'tier' in object.metadata.labels) || object.metadata.labels.tier != params.data.forbidden"),
		paramsBinding(&admissionregv1.ParamRef{
			Namespace:               "params",
			Selector:                &metav1.LabelSelector{MatchLabels: selected},
			ParameterNotFoundAction: new(admissionregv1.DenyAction),
		}),
	})

	eventually(t, time.Minute, func() error { return recordedExactly(ctx, c, []string{"bronze", "copper"}) })
}

// TestParamChangeReevaluates checks that an update to only the params object re-evaluates the objects
// that read it, so a compliant object that now violates exempts its namespace.
func TestParamChangeReevaluates(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("waits about a minute: a params change replaces the informer only once it is one resync interval old")
	}

	c := startEnv(t)
	ctx := t.Context()

	param := tierParam("params", "tier", nil, map[string]string{"tier": "gold"})

	createAll(t, c, []client.Object{
		&corev1.Namespace{Name: "params"},
		param,
		fixtureNS("gold"), tiered("gold", "gold"),
		fixtureNS("silver"), tiered("silver", "silver"),
		paramsPolicy(configMapParams(), tierMatches),
		paramsBinding(&admissionregv1.ParamRef{Name: "tier", Namespace: "params", ParameterNotFoundAction: new(admissionregv1.DenyAction)}),
	})

	eventually(t, time.Minute, func() error { return recordedExactly(ctx, c, []string{"silver"}) })

	param.Data["tier"] = "silver"
	if err := c.Update(ctx, param); err != nil {
		t.Fatalf("update params: %v", err)
	}

	// silver stays exempt until its TTL expires, so gold is added.
	eventually(t, 150*time.Second, func() error { return recordedExactly(ctx, c, []string{"gold", "silver"}) })
}

// TestParamNotFound checks that a missing params object is handled by the paramRef's
// parameterNotFoundAction, and that its Deny is a configuration error the policy's failurePolicy decides.
func TestParamNotFound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		action  admissionregv1.ParameterNotFoundActionType
		failure admissionregv1.FailurePolicyType
		want    []string
	}{
		{name: "Deny", action: admissionregv1.DenyAction, failure: admissionregv1.Fail, want: []string{"app"}},
		{name: "Allow", action: admissionregv1.AllowAction, failure: admissionregv1.Fail, want: []string{}},
		{name: "Deny with failurePolicy Ignore", action: admissionregv1.DenyAction, failure: admissionregv1.Ignore, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := startEnv(t)
			ctx := t.Context()

			policy := paramsPolicy(configMapParams(), tierMatches)
			policy.Spec.FailurePolicy = new(tc.failure)

			createAll(t, c, []client.Object{
				fixtureNS("app"), tiered("app", "gold"),
				policy,
				paramsBinding(&admissionregv1.ParamRef{Name: "missing", Namespace: "params", ParameterNotFoundAction: new(tc.action)}),
			})

			eventually(t, time.Minute, func() error { return recordedExactly(ctx, c, tc.want) })
		})
	}
}

// tierCRD is a cluster-scoped params kind.
func tierCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		Name: "tiers.kar.test",
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "kar.test",
			Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "tiers", Singular: "tier", Kind: "Tier", ListKind: "TierList"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: new(true)}},
			}},
		},
	}
}

// TestParamClusterScoped checks that a cluster-scoped params kind is read without a namespace, for objects
// in every namespace.
func TestParamClusterScoped(t *testing.T) {
	t.Parallel()

	cfg := startAPIServer(t, nil)
	c := newClient(t, cfg)
	ctx := t.Context()

	// The CRD must be served before the controller's first discovery.
	createAll(t, c, []client.Object{tierCRD()})

	eventually(t, time.Minute, func() error {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(ctx, client.ObjectKey{Name: "tiers.kar.test"}, crd); err != nil {
			return fmt.Errorf("get CRD: %w", err)
		}

		if !slices.ContainsFunc(crd.Status.Conditions, func(cond apiextensionsv1.CustomResourceDefinitionCondition) bool {
			return cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue
		}) {
			return errNotEstablished
		}

		return nil
	})

	tier := &unstructured.Unstructured{}
	tier.SetGroupVersionKind(schema.GroupVersionKind{Group: "kar.test", Version: "v1", Kind: "Tier"})
	tier.SetName("default")

	if err := unstructured.SetNestedField(tier.Object, "gold", "spec", "tier"); err != nil {
		t.Fatalf("set tier: %v", err)
	}

	createAll(t, c, []client.Object{
		tier,
		fixtureNS("gold"), tiered("gold", "gold"),
		fixtureNS("silver"), tiered("silver", "silver"),
		paramsPolicy(&admissionregv1.ParamKind{APIVersion: "kar.test/v1", Kind: "Tier"},
			"has(object.metadata.labels) && 'tier' in object.metadata.labels && object.metadata.labels.tier == params.spec.tier"),
		paramsBinding(&admissionregv1.ParamRef{Name: "default", ParameterNotFoundAction: new(admissionregv1.DenyAction)}),
	})

	startController(t, cfg)

	eventually(t, time.Minute, func() error { return recordedExactly(ctx, c, []string{"silver"}) })
}
