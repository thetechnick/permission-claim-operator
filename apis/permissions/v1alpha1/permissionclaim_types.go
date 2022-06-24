package v1alpha1

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PermissionClaimSpec defines the desired state of a PermissionClaim.
type PermissionClaimSpec struct {
	// Namespace to claim permissions in.
	// This is the namespace that the ServiceAccount and namespaced-scoped Roles will live.
	Namespace string `json:"namespace"`
	// Name of the secret to house the created credentials.
	SecretName string `json:"secretName"`
	// Namespace-scoped permissions.
	Rules []rbacv1.PolicyRule `json:"rules,omitempty"`
	// Cluster-scoped permissions.
	ClusterRules []rbacv1.PolicyRule `json:"clusterRules,omitempty"`
}

// PermissionClaimStatus defines the observed state of a PermissionClaim
type PermissionClaimStatus struct {
	// Conditions is a list of status conditions ths object is in.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// DEPRECATED: This field is not part of any API contract
	// it will go away as soon as kubectl can print conditions!
	// Human readable status - please use .Conditions from code
	Phase PermissionClaimPhase `json:"phase,omitempty"`
}

const (
	PermissionClaimBound = "Bound"
)

type PermissionClaimPhase string

// Well-known PermissionClaim Phases for printing a Status in kubectl,
// see deprecation notice in PermissionClaimStatus for details.
const (
	PermissionClaimPhasePending PermissionClaimPhase = "Pending"
	PermissionClaimPhaseBound   PermissionClaimPhase = "Bound"
)

// PermissionClaim controls the handover process between two operators.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Found",type="integer",JSONPath=".status.stats.found"
// +kubebuilder:printcolumn:name="Available",type="integer",JSONPath=".status.stats.available"
// +kubebuilder:printcolumn:name="Updated",type="integer",JSONPath=".status.stats.updated"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type PermissionClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PermissionClaimSpec `json:"spec,omitempty"`
	// +kubebuilder:default={phase:Pending}
	Status PermissionClaimStatus `json:"status,omitempty"`
}

// PermissionClaimList contains a list of PermissionClaims
// +kubebuilder:object:root=true
type PermissionClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PermissionClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PermissionClaim{}, &PermissionClaimList{})
}
