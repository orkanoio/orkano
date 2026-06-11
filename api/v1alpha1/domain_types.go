package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionCertificateReady mirrors the cert-manager Certificate Ready
// condition onto the Domain that caused its issuance.
const ConditionCertificateReady = "CertificateReady"

type DomainSpec struct {
	// Host is immutable: re-pointing a hostname is delete-and-recreate,
	// which sidesteps certificate and Ingress rename edge cases
	// (ADR-0006).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="host is immutable; re-pointing a hostname is delete-and-recreate"
	Host string `json:"host"`

	// AppRef is immutable for the same reason as host: re-pointing leaves
	// no event that would ever heal the old App's derived status.url.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="appRef is immutable; re-pointing a domain is delete-and-recreate"
	AppRef LocalObjectRef `json:"appRef"`
}

type DomainStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carry Ready and CertificateReady; host conflicts
	// surface as Ready=False with reason HostConflict.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=orkano
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 249",message="domain name must be at most 249 characters so the derived TLS secret name fits the 253-character limit"
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.host`
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.appRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type Domain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DomainSpec   `json:"spec,omitempty"`
	Status DomainStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type DomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Domain `json:"items"`
}
