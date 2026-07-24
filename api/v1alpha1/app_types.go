package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type WorkloadType string

const (
	WorkloadWeb    WorkloadType = "Web"
	WorkloadWorker WorkloadType = "Worker"
)

// ConditionReady is the summary condition on every Orkano kind.
const ConditionReady = "Ready"

// ConditionReleasePinned is True whenever spec.pinnedBuild is set — so
// meta.IsStatusConditionTrue answers "is automatic deploy paused?", the
// question that actually matters. The reason discriminates a healthy pin
// (Pinned) from a broken one (PinnedBuildNotFound, PinnedBuildNotSucceeded).
const ConditionReleasePinned = "ReleasePinned"

// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type != 'Worker' || (!has(self.port) && !has(self.healthCheck))",message="Worker apps cannot set port or healthCheck"
// +kubebuilder:validation:XValidation:rule="has(self.source.image) == (self.build.strategy == 'Image')",message="an image source requires build.strategy Image, and build.strategy Image requires an image source"
// +kubebuilder:validation:XValidation:rule="!has(self.volumes) || size(self.volumes) == 0 || !has(self.replicas) || self.replicas <= 1",message="an app with volumes cannot exceed one replica: the volume is ReadWriteOnce, so a second pod either blocks on Multi-Attach or writes the same data concurrently"
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

	// Volumes are persistent directories mounted into the app. Declaring any
	// forces the Deployment to the Recreate strategy and caps replicas at one:
	// the claims are ReadWriteOnce, and a rolling update's surge pod either
	// blocks forever on Multi-Attach or — on a node-affine local volume —
	// successfully mounts the same directory alongside the outgoing pod.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Volumes []AppVolume `json:"volumes,omitempty"`

	// PinnedBuild holds this app on one Build's image instead of tracking the
	// newest successful one — the release selector behind "roll back". It names
	// a Build, never an image: the digest is only ever read from that Build's
	// status, so no caller can point an app at an arbitrary image. Clearing it
	// resumes automatic deploys.
	//
	// Deliberately no MinLength: with omitempty no Go client serializes "", so
	// the only value it could reject is a hand-written
	// `kubectl patch -p '{"spec":{"pinnedBuild":""}}'` — the natural unpin.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	PinnedBuild string `json:"pinnedBuild,omitempty"`
}

// AppVolume is one persistent directory. The claim is created by the operator
// and owned by the App, so deleting the App deletes the data — the same
// contract the service catalog states (ADR-0014).
type AppVolume struct {
	// Name identifies the volume within the app and derives its claim name
	// (`<app>-<name>`), so it carries DNS-1123 label rules.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// MountPath is the absolute in-container path. It may not be "/", and the
	// no-".." rule mirrors volumeMount.subPath's.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^/[A-Za-z0-9_./-]*[A-Za-z0-9_-]$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="mountPath must not contain '..'"
	MountPath string `json:"mountPath"`

	// Size is the requested capacity. It may grow but never shrink — a
	// PersistentVolumeClaim expands in place and cannot be reduced.
	// +kubebuilder:default="1Gi"
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`
}

// AppVolumeStatus echoes what was actually provisioned, so a user can tell a
// requested size from a bound one without reading PersistentVolumeClaims.
type AppVolumeStatus struct {
	Name string `json:"name"`

	// ClaimName is the PersistentVolumeClaim backing this volume.
	ClaimName string `json:"claimName"`

	// +optional
	Phase string `json:"phase,omitempty"`

	// Capacity is the bound size, which may exceed the request.
	// +optional
	Capacity *resource.Quantity `json:"capacity,omitempty"`
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

	// Volumes echoes what was provisioned for spec.volumes.
	// +listType=map
	// +listMapKey=name
	// +optional
	Volumes []AppVolumeStatus `json:"volumes,omitempty"`
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
