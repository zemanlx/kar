package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"slices"
	"strings"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	modeAnnotation = "kar.zeman.dev/mode"
	ttlAnnotation  = "kar.zeman.dev/ttl"
	modeObserve    = "observe"
	modeEnforce    = "enforce"
)

// derivedName returns the name of source's derived binding. A name too long for that is shortened and
// given a hash of the full name, so distinct sources keep distinct names.
func derivedName(source string) string {
	if name := source + "-enforce"; len(name) <= validation.DNS1123SubdomainMaxLength {
		return name
	}

	sum := sha256.Sum256([]byte(source))
	suffix := "-" + hex.EncodeToString(sum[:4]) + "-enforce"
	prefix := strings.TrimRight(source[:validation.DNS1123SubdomainMaxLength-len(suffix)], ".-")

	return prefix + suffix
}

// mode returns b's kar.zeman.dev/mode, or "" when it has none.
func mode(b *admissionregv1.ValidatingAdmissionPolicyBinding) string {
	return b.Annotations[modeAnnotation]
}

// ttlOf returns b's kar.zeman.dev/ttl, or def when it is missing or not a positive duration.
func ttlOf(b *admissionregv1.ValidatingAdmissionPolicyBinding, def time.Duration) time.Duration {
	d, err := time.ParseDuration(b.Annotations[ttlAnnotation])
	if err != nil || d <= 0 {
		return def
	}

	return d
}

// derive returns the Deny binding for source that exempts the namespaces in exempt. It never
// mutates source and copies no annotations, so the derived binding is not itself opted in.
func derive(source *admissionregv1.ValidatingAdmissionPolicyBinding, exempt []string) *admissionregv1.ValidatingAdmissionPolicyBinding {
	out := &admissionregv1.ValidatingAdmissionPolicyBinding{
		APIVersion:      admissionregv1.SchemeGroupVersion.String(),
		Kind:            "ValidatingAdmissionPolicyBinding",
		Name:            derivedName(source.Name),
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(source, admissionregv1.SchemeGroupVersion.WithKind("ValidatingAdmissionPolicyBinding"))},
		Spec:            denySemantics(source).Spec,
	}

	if len(exempt) > 0 {
		if out.Spec.MatchResources == nil {
			out.Spec.MatchResources = &admissionregv1.MatchResources{}
		}

		if out.Spec.MatchResources.NamespaceSelector == nil {
			out.Spec.MatchResources.NamespaceSelector = &metav1.LabelSelector{}
		}

		sel := out.Spec.MatchResources.NamespaceSelector
		sel.MatchExpressions = append(sel.MatchExpressions, metav1.LabelSelectorRequirement{
			Key:      corev1.LabelMetadataName,
			Operator: metav1.LabelSelectorOpNotIn,
			Values:   slices.Clone(exempt),
		})
	}

	return out
}

// denySemantics returns a copy of source whose only validation action is Deny. The audit evaluates
// with Deny semantics, since a Warn source would otherwise always allow.
func denySemantics(source *admissionregv1.ValidatingAdmissionPolicyBinding) *admissionregv1.ValidatingAdmissionPolicyBinding {
	out := source.DeepCopy()
	out.Spec.ValidationActions = []admissionregv1.ValidationAction{admissionregv1.Deny}

	return out
}

// karExpression returns the namespace expression kar appended to derived, or nil when there is
// none. derive always appends kar's expression last, so it is derived's last expression when that
// is a metadata.name NotIn that source does not also carry. The source may have changed since
// derived was written, so the expression is matched by content, not by position or count.
func karExpression(derived, source *admissionregv1.ValidatingAdmissionPolicyBinding) *metav1.LabelSelectorRequirement {
	exprs := namespaceExprs(derived)
	if len(exprs) == 0 {
		return nil
	}

	last := exprs[len(exprs)-1]
	if last.Key != corev1.LabelMetadataName || last.Operator != metav1.LabelSelectorOpNotIn {
		return nil
	}

	if slices.ContainsFunc(namespaceExprs(source), func(e metav1.LabelSelectorRequirement) bool {
		return reflect.DeepEqual(e, last)
	}) {
		return nil
	}

	return &last
}

// namespaceExprs returns b's namespace selector expressions, or nil when it has none.
func namespaceExprs(b *admissionregv1.ValidatingAdmissionPolicyBinding) []metav1.LabelSelectorRequirement {
	if b == nil || b.Spec.MatchResources == nil || b.Spec.MatchResources.NamespaceSelector == nil {
		return nil
	}

	return b.Spec.MatchResources.NamespaceSelector.MatchExpressions
}
