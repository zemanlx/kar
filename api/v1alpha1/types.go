// Package v1alpha1 contains the kar.zeman.dev API types.
//
// +groupName=kar.zeman.dev
// +kubebuilder:object:generate=true
package v1alpha1

//go:generate go tool controller-gen object paths=./... crd paths=./... output:crd:dir=../../charts/kar/crds

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group and version of the kar types.
//
//nolint:gochecknoglobals // schema.GroupVersion is a struct and cannot be a const.
var GroupVersion = schema.GroupVersion{Group: "kar.zeman.dev", Version: "v1alpha1"}

// AddToScheme registers the kar types with a scheme.
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &PolicyExemption{}, &PolicyExemptionList{})
	metav1.AddToGroupVersion(s, GroupVersion)

	return nil
}

// Condition types and reasons set on PolicyExemption status.
const (
	ConditionSynced    = "Synced"
	ConditionAuditable = "Auditable"
	ConditionEnforcing = "Enforcing"

	ReasonStateTooLarge = "StateTooLarge"
	ReasonObserve       = "Observe"
	ReasonCRDOutdated   = "CRDOutdated"

	ReasonPolicyNotFound  = "PolicyNotFound"
	ReasonNotAuditable    = "NotAuditable"
	ReasonAuditable       = "Auditable"
	ReasonDiscoveryFailed = "DiscoveryFailed"
	ReasonInformerFailed  = "InformerFailed"
	ReasonInformerSyncing = "InformerSyncing"
	ReasonReadFailed      = "ReadFailed"
	ReasonSynced          = "Synced"
	ReasonApplyFailed     = "ApplyFailed"
	ReasonEnforcing       = "Enforcing"
)

// PolicyExemption lists the namespaces exempted from a generated Deny binding.
// It has no spec; the controller writes everything to status.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Enforcing",type="string",JSONPath=".status.conditions[?(@.type==\"Enforcing\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type PolicyExemption struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Status PolicyExemptionStatus `json:"status,omitzero"`
}

// PolicyExemptionStatus is the observed state of a PolicyExemption.
type PolicyExemptionStatus struct {
	// +listType=map
	// +listMapKey=name
	Namespaces []ExemptNamespace `json:"namespaces,omitempty"`
	// ObservingSince is when the observation window of an auto binding started.
	ObservingSince *metav1.Time `json:"observingSince,omitempty"`
	// ObservedInputs is "policyGeneration/bindingGeneration" when the observation window started.
	ObservedInputs string `json:"observedInputs,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ExemptNamespace is a namespace currently exempted and when its exemption expires.
type ExemptNamespace struct {
	Name       string      `json:"name"`
	FirstSeen  metav1.Time `json:"firstSeen"`
	Expiration metav1.Time `json:"expiration"`
}

// PolicyExemptionList is a list of PolicyExemption objects.
//
// +kubebuilder:object:root=true
type PolicyExemptionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`

	Items []PolicyExemption `json:"items"`
}
