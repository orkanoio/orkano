package v1alpha1

import "k8s.io/apimachinery/pkg/api/resource"

type Source struct {
	GitHub GitHubSource `json:"github"`

	// SubPath scopes the build context to a directory of the checkout,
	// like volumeMount.subPath; the Dockerfile path resolves relative
	// to it. The pattern and the no-".." rule mirror volumeMount.subPath:
	// the value lands in the BuildKit git context URL, where "#" or ":"
	// would change which ref/directory is built and ".." would escape
	// the intended directory.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_./-]+$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="subPath must not contain '..'"
	// +optional
	SubPath string `json:"subPath,omitempty"`
}

type GitHubSource struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	// +kubebuilder:validation:MaxLength=140
	Repo string `json:"repo"`

	// Ref has no schema default: a baked-in "main" would break "master"
	// repos forever once persisted. Unset means the repo's default
	// branch, resolved by the operator at build time.
	// +kubebuilder:validation:MaxLength=250
	// +optional
	Ref string `json:"ref,omitempty"`
}

// Build strategy values, kept in sync with the Strategy enum below.
const (
	StrategyDockerfile = "Dockerfile"
	StrategyStatic     = "Static"
)

// +kubebuilder:validation:XValidation:rule="self.strategy == 'Dockerfile' ? !has(self.static) : (has(self.static) && !has(self.dockerfile))",message="build members must match the chosen strategy"
type BuildStrategy struct {
	// +kubebuilder:validation:Enum=Dockerfile;Static
	Strategy string `json:"strategy"`

	// +optional
	Dockerfile *DockerfileBuild `json:"dockerfile,omitempty"`

	// +optional
	Static *StaticBuild `json:"static,omitempty"`
}

// DockerfileBuild is deliberately path-only in v1alpha1: buildArgs and
// target were deferred by ADR-0012 after the BuildKit spike — each widens
// the hostile-input surface without being needed for the core loop.
type DockerfileBuild struct {
	// Path to the Dockerfile, relative to source.subPath when set; an omitted
	// path (or omitted dockerfile block) selects the default "Dockerfile". The
	// pattern and the no-".." rule mirror source.subPath and static.dir: the
	// value lands in BuildKit's --opt=filename argument, where ".." would point
	// the build outside the intended subdirectory of the (commit-pinned)
	// context. Lower-risk than its siblings — single argv, no shell, no context
	// escape — but closing the validation asymmetry is cheap.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_./-]+$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="path must not contain '..'"
	// +optional
	Path string `json:"path,omitempty"`
}

type StaticBuild struct {
	// Dir is the directory of build output to serve, relative to the
	// source root. It is COPYed into a generated Dockerfile, so it carries
	// source.subPath's pattern + no-".." rule: a newline would inject a
	// Dockerfile instruction, and ".." would escape the build context.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_./-]+$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="dir must not contain '..'"
	Dir string `json:"dir"`
}

// +kubebuilder:validation:XValidation:rule="has(self.value) != has(self.secretRef)",message="exactly one of value or secretRef must be set"
type EnvVar struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +optional
	Value string `json:"value,omitempty"`

	// +optional
	SecretRef *SecretKeyRef `json:"secretRef,omitempty"`
}

// SecretKeyRef names a key in a Kubernetes Secret. Only references ever
// appear in Orkano CRs; values live exclusively in Secrets (INV-03).
type SecretKeyRef struct {
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// Resources maps to container requests; the operator derives limits
// (memory limit = request, no CPU limit) so those defaults can improve
// without a stored-object migration.
type Resources struct {
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// HealthCheck set means HTTP readiness and liveness probes on Path with
// fixed sane timings; unset means a TCP readiness probe on the port.
type HealthCheck struct {
	// +kubebuilder:validation:Pattern=`^/.*$`
	// +kubebuilder:validation:MaxLength=512
	Path string `json:"path"`
}

type LocalObjectRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}
