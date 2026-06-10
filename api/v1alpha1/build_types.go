package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type BuildPhase string

const (
	BuildPending   BuildPhase = "Pending"
	BuildRunning   BuildPhase = "Running"
	BuildSucceeded BuildPhase = "Succeeded"
	BuildFailed    BuildPhase = "Failed"
)

// BuildSpec is a record of one build attempt: source and strategy are
// snapshots taken from the App at trigger time, so the record stays honest
// after the App changes. Retrying means creating a new Build.
type BuildSpec struct {
	// +kubebuilder:validation:MaxLength=253
	AppName string `json:"appName"`

	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Commit string `json:"commit"`

	Source Source `json:"source"`

	Strategy BuildStrategy `json:"strategy"`

	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=3600
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

type BuildStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	// +optional
	Phase BuildPhase `json:"phase,omitempty"`

	// Image is the digest-pinned reference the build pushed.
	// +optional
	Image string `json:"image,omitempty"`

	// JobRef points at the BuildKit Job (in the build namespace, hence
	// namespace+name); in Phase 1 its pod logs are the build log.
	// +optional
	JobRef *JobReference `json:"jobRef,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type JobReference struct {
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=orkano
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.appName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.spec.commit`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type Build struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BuildSpec   `json:"spec,omitempty"`
	Status BuildStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type BuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Build `json:"items"`
}
