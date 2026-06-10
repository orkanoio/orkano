package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type AppSpikeSpec struct {
	// +kubebuilder:validation:MinLength=1
	Message string `json:"message"`
}

type AppSpikeStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type AppSpike struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppSpikeSpec   `json:"spec,omitempty"`
	Status AppSpikeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type AppSpikeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppSpike `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AppSpike{}, &AppSpikeList{})
}
