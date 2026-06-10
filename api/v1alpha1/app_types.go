package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type WorkloadType string

const (
	WorkloadWeb    WorkloadType = "Web"
	WorkloadWorker WorkloadType = "Worker"
)

// ConditionReady is the summary condition on every Orkano kind.
const ConditionReady = "Ready"

type AppSpec struct {
	Source Source `json:"source"`

	Build BuildStrategy `json:"build"`

	// +kubebuilder:validation:Enum=Web;Worker
	// +kubebuilder:default=Web
	// +optional
	Type WorkloadType `json:"type,omitempty"`

	// Command overrides the image entrypoint.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Command []string `json:"command,omitempty"`

	// Port the container listens on. No schema default: defaulting here
	// would inject a port into Worker apps. The operator defaults Web
	// apps to 8080 at render time and injects PORT to match.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`

	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Env []EnvVar `json:"env,omitempty"`

	// +optional
	Resources *Resources `json:"resources,omitempty"`

	// +optional
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`
}

type AppStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Image is the digest-pinned reference currently rolled out.
	// +optional
	Image string `json:"image,omitempty"`

	// URL derived from Domains pointing at this App.
	// +optional
	URL string `json:"url,omitempty"`

	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// LatestBuild names the most recent Build for this App.
	// +optional
	LatestBuild string `json:"latestBuild,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=orkano
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type App struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppSpec   `json:"spec,omitempty"`
	Status AppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []App `json:"items"`
}
