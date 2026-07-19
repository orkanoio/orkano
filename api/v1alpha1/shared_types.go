package v1alpha1

import "k8s.io/apimachinery/pkg/api/resource"

// +kubebuilder:validation:XValidation:rule="has(self.github) ? (!has(self.git) && !has(self.upload)) : (has(self.git) != has(self.upload))",message="exactly one of github, git, or upload must be set"
type Source struct {
	// +optional
	GitHub *GitHubSource `json:"github,omitempty"`

	// +optional
	Git *GitSource `json:"git,omitempty"`

	// +optional
	Upload *UploadSource `json:"upload,omitempty"`

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

// GitSource describes an unauthenticated HTTPS Git repository. Generic Git is
// manual-deploy-only: automatic push delivery remains a GitHub App capability.
// The feature gate and public-address checks are enforced by every component
// before it performs network I/O; the CRD schema rejects credentials, query
// parameters, fragments, and non-HTTPS URLs at the API boundary.
type GitSource struct {
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[A-Za-z0-9.-]+(:443)?/[A-Za-z0-9._~!$&'()*+,;=:%/-]+$`
	URL string `json:"url"`

	// Ref is a branch, tag, or commit-like name. Unset means the repository's
	// advertised HEAD. It is resolved to an immutable commit before a Build is
	// created.
	// +kubebuilder:validation:MaxLength=250
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_./-]+$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="ref must not contain '..'"
	// +optional
	Ref string `json:"ref,omitempty"`
}

// UploadSource identifies an immutable ZIP artifact stored in Orkano's
// in-cluster OCI registry. FileName is display-only and never a filesystem
// destination.
type UploadSource struct {
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`

	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,250}[.]zip$`
	// +optional
	FileName string `json:"fileName,omitempty"`
}

// Build strategy values, kept in sync with the Strategy enum below.
const (
	StrategyDockerfile = "Dockerfile"
	StrategyStatic     = "Static"
	StrategyNixpacks   = "Nixpacks"
)

// +kubebuilder:validation:XValidation:rule="self.strategy == 'Dockerfile' ? (!has(self.static) && !has(self.nixpacks)) : (self.strategy == 'Static' ? (has(self.static) && !has(self.dockerfile) && !has(self.nixpacks)) : (has(self.nixpacks) && !has(self.dockerfile) && !has(self.static)))",message="build members must match the chosen strategy"
type BuildStrategy struct {
	// +kubebuilder:validation:Enum=Dockerfile;Static;Nixpacks
	Strategy string `json:"strategy"`

	// +optional
	Dockerfile *DockerfileBuild `json:"dockerfile,omitempty"`

	// +optional
	Static *StaticBuild `json:"static,omitempty"`

	// +optional
	Nixpacks *NixpacksBuild `json:"nixpacks,omitempty"`
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

// NixpacksBuild configures Dockerfile generation by Nixpacks before the
// existing rootless BuildKit build. Nixpacks never receives a Docker socket.
// An empty block uses Nixpacks' conventional repository configuration.
type NixpacksBuild struct {
	// ConfigPath optionally selects a Nixpacks TOML file relative to
	// source.subPath.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_./-]+$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="configPath must not contain '..'"
	// +optional
	ConfigPath string `json:"configPath,omitempty"`
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
