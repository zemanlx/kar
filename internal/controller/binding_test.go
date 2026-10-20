package controller

import (
	"reflect"
	"strings"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func sourceWith(exprs []metav1.LabelSelectorRequirement) *admissionregv1.ValidatingAdmissionPolicyBinding {
	b := &admissionregv1.ValidatingAdmissionPolicyBinding{
		Name:        "src",
		UID:         "uid-1",
		Annotations: map[string]string{"kar.zeman.dev/mode": "enforce"},
		Spec: admissionregv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        "pol",
			ValidationActions: []admissionregv1.ValidationAction{admissionregv1.Warn},
		},
	}
	if exprs != nil {
		b.Spec.MatchResources = &admissionregv1.MatchResources{
			NamespaceSelector: &metav1.LabelSelector{MatchExpressions: exprs},
		}
	}

	return b
}

// checkDerived asserts the fields of derived that do not depend on the exemptions.
func checkDerived(t *testing.T, source, derived *admissionregv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()

	if derived.TypeMeta != (metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingAdmissionPolicyBinding"}) {
		t.Errorf("TypeMeta = %+v", derived.TypeMeta)
	}

	if derived.Name != "src-enforce" || len(derived.Labels) != 0 || len(derived.Annotations) != 0 {
		t.Errorf("name %q labels %v annotations %v", derived.Name, derived.Labels, derived.Annotations)
	}

	if want := []admissionregv1.ValidationAction{admissionregv1.Deny}; !reflect.DeepEqual(derived.Spec.ValidationActions, want) {
		t.Errorf("ValidationActions = %v, want %v", derived.Spec.ValidationActions, want)
	}

	if derived.Spec.PolicyName != source.Spec.PolicyName {
		t.Errorf("PolicyName = %q, want %q", derived.Spec.PolicyName, source.Spec.PolicyName)
	}

	owner := metav1.OwnerReference{
		APIVersion:         "admissionregistration.k8s.io/v1",
		Kind:               "ValidatingAdmissionPolicyBinding",
		Name:               "src",
		UID:                "uid-1",
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}
	if !reflect.DeepEqual(derived.OwnerReferences, []metav1.OwnerReference{owner}) {
		t.Errorf("OwnerReferences = %+v, want %+v", derived.OwnerReferences, owner)
	}
}

func TestDerive(t *testing.T) {
	t.Parallel()

	env := metav1.LabelSelectorRequirement{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}}
	human := metav1.LabelSelectorRequirement{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"h"}}
	kar := func(exempt ...string) metav1.LabelSelectorRequirement {
		return metav1.LabelSelectorRequirement{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: exempt}
	}

	exprs := func(r ...metav1.LabelSelectorRequirement) []metav1.LabelSelectorRequirement { return r }

	tests := []struct {
		name   string
		src    []metav1.LabelSelectorRequirement
		exempt []string
		want   []metav1.LabelSelectorRequirement
	}{
		{name: "no exempt keeps source expressions", src: exprs(env), want: exprs(env)},
		{name: "exempt appends kar expression last", src: exprs(env), exempt: []string{"a", "b"}, want: exprs(env, kar("a", "b"))},
		{name: "source's human NotIn stays before kar", src: exprs(human), exempt: []string{"a"}, want: exprs(human, kar("a"))},
		{name: "no source selector gets one", exempt: []string{"a"}, want: exprs(kar("a"))},
		{name: "no source selector and no exempt stays nil"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			source := sourceWith(tt.src)
			before := source.DeepCopy()

			derived := derive(source, tt.exempt)
			checkDerived(t, source, derived)

			if !reflect.DeepEqual(source, before) {
				t.Errorf("derive() mutated source:\n got %+v\nwant %+v", source, before)
			}

			var got []metav1.LabelSelectorRequirement
			if mr := derived.Spec.MatchResources; mr != nil && mr.NamespaceSelector != nil {
				got = mr.NamespaceSelector.MatchExpressions
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("matchExpressions = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDerivedName(t *testing.T) {
	t.Parallel()

	if got := derivedName("src"); got != "src-enforce" {
		t.Errorf("derivedName(src) = %q, want src-enforce", got)
	}

	// 245 characters is the longest source whose derived name still fits in 253.
	fits := strings.Repeat("a", 245)
	if got := derivedName(fits); got != fits+"-enforce" {
		t.Errorf("derivedName(245 characters) = %q, want it unchanged", got)
	}

	// The cut falls on the dot, which the shortened name must not end with.
	dotted := strings.Repeat("a", 235) + "." + strings.Repeat("b", 17)

	for _, src := range []string{strings.Repeat("a", 250), dotted} {
		got := derivedName(src)
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
			t.Errorf("derivedName(%d characters) = %q: %v", len(src), got, errs)
		}

		if again := derivedName(src); again != got {
			t.Errorf("derivedName(%d characters) = %q, then %q", len(src), got, again)
		}
	}

	long := derivedName(strings.Repeat("a", 250))
	if sibling := derivedName(strings.Repeat("a", 249) + "b"); sibling == long {
		t.Errorf("names sharing a prefix got the same derived name %q", long)
	}
}

func TestTTLOf(t *testing.T) {
	t.Parallel()

	def := 192 * time.Hour

	tests := []struct {
		name string
		ttl  string
		want time.Duration
	}{
		{name: "missing", want: def},
		{name: "valid", ttl: "24h", want: 24 * time.Hour},
		{name: "invalid", ttl: "soon", want: def},
		{name: "not positive", ttl: "0s", want: def},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := sourceWith(nil)
			if tt.ttl != "" {
				b.Annotations[ttlAnnotation] = tt.ttl
			}

			if got := ttlOf(b, def); got != tt.want {
				t.Errorf("ttlOf() = %v, want %v", got, tt.want)
			}
		})
	}
}
