// Package buildjob composes the BuildKit invocation for a Build (Compose) and
// renders the Job that runs it as rootless BuildKit (Render).
//
// The Job shape is spike attempt F2 verbatim — the minimal configuration PSA
// baseline admits with zero warnings (ADR-0012) — plus the two product
// deltas: the build context is a git URL fetched over the egress
// allowlist's 443 rule, and the push target is the TLS registry, trusted
// through the cluster-internal CA (registry.insecure never ships).
// Both Compose and Render are pure: the Build controller owns creation,
// status, and cleanup. A golden copy of the rendered Job is pinned at
// hack/ci/substrate-smoke/09-build-job-template.yaml, where the substrate
// smoke runs it end to end under the full lockdown.
package buildjob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

const (
	// Namespace is the one namespace build Jobs run in (ADR-0005). Its PSA
	// level and the config/netpol/ lockdown are this template's other half.
	Namespace = "orkano-builds"

	// RegistryHost is the canonical, portless image host (config/registry/):
	// exactly one string form exists for the future INV-06 policy to match.
	RegistryHost = "orkano-registry.orkano-system.svc.cluster.local"

	// DefaultImage is the rootless BuildKit the spike proved, digest-pinned.
	// On a bump, run `make verify-image-pins` to confirm the new digest is a
	// multi-arch index (amd64+arm64), not a single-platform manifest.
	DefaultImage = "moby/buildkit:v0.30.0-rootless@sha256:d76eb1caecac5733ef7553c1e90a1b21f1bb218cd1142d3553de0747b4a14ba9"

	// DefaultGitImage performs the commit-pinned checkout needed by local
	// contexts (ZIP parity and Nixpacks). It is multi-arch and contains only
	// Alpine plus Git; no credential is mounted into it.
	DefaultGitImage = "alpine/git:2.49.1@sha256:c0280cf9572316299b08544065d3bf35db65043d5e3963982ec50647d2746e26"

	// DefaultNixpacksImage is Nixpacks' official multi-arch Ubuntu/Nix base.
	// The init container downloads the v1.41.0 CLI release and verifies the
	// architecture-specific SHA before execution. Nixpacks is default-off and
	// maintenance-mode, which is why the explicit build.nixpacks gate exists.
	DefaultNixpacksImage = "ghcr.io/railwayapp/nixpacks:latest@sha256:fafdd33327c3c824402c7ac5dc93110da2613ef2a6ba9c5390a8c34c241e548f"

	// podLabelKey/Value is the NetworkPolicy contract (config/netpol/): a
	// pod without this label gets no network in orkano-builds, fail closed.
	podLabelKey   = "app.kubernetes.io/name"
	podLabelValue = "orkano-build"

	// serviceAccountName is the no-permission, no-token SA from config/rbac
	// (INV-02). Naming it beats the namespace default SA: the explicit
	// automountServiceAccountToken stays, and the RBAC matrix row about
	// build pods points at an SA the Job actually uses.
	serviceAccountName = "orkano-build"

	// AnnotationBuildName/Namespace map a Job back to the Build it runs.
	// Builds and Jobs live in different namespaces, and Kubernetes forbids
	// cross-namespace ownerReferences, so these annotations are the link the
	// Build controller's watch inverts (and its foreign-Job refusal checks).
	AnnotationBuildName      = "orkano.io/build-name"
	AnnotationBuildNamespace = "orkano.io/build-namespace"

	// CAConfigMapName is published at install from the registry TLS Secret's
	// ca.crt (M1.5 contract); the smoke's TLS probe uses the same projection,
	// and the operator's digest resolver reads the same bundle.
	CAConfigMapName = "orkano-registry-ca"
	caMountPath     = "/orkano-registry-ca"

	// configConfigMapName carries buildkitd.toml (config/buildkit/), which
	// points BuildKit's registry client at the projected CA; a test pins
	// that manifest against these constants.
	configConfigMapName = "orkano-buildkit-config"
	configMountPath     = "/orkano-buildkit-config"

	appArmorProfileName = "orkano-buildkit"

	// DefaultTimeoutSeconds mirrors the CRD's timeoutSeconds default; the
	// Build controller quotes it in timeout failure messages, so the two
	// must not drift.
	DefaultTimeoutSeconds = 900
)

const (
	// DefaultDockerfile is BuildKit's default Dockerfile name, composed when a
	// Dockerfile build omits build.dockerfile — the valid CEL edge of
	// BuildStrategy (Dockerfile strategy with no dockerfile block, e.g.
	// examples 02/03/04).
	DefaultDockerfile = "Dockerfile"

	// DefaultGitBaseURL is the base a Build clones the App's repo from when the
	// operator's --git-base-url is unset. GitHubSource.Repo is "<owner>/<name>",
	// so Compose forms "<base><owner>/<name>.git#<commit>"; BuildKit fetches over
	// the orkano-builds egress allowlist's tcp/443 rule. The hermetic E2E (and an
	// air-gapped mirror) override the base with an in-cluster git server — the
	// value must end in "/", validated where the operator flag is parsed.
	DefaultGitBaseURL = "https://github.com/"

	// StaticServerImage serves a Static app's build output. nginx-unprivileged
	// runs non-root (UID 101), listens on 8080 (= the App's default web port,
	// so a static site needs zero port config), serves /usr/share/nginx/html
	// with readOnlyRootFilesystem support out of the box, is multi-arch and
	// Apache-2.0. ADR-0007 governs the PRODUCT images Orkano publishes, not a
	// user-app base image like this; applying its spirit (non-root, official
	// provenance, digest-pinnable, permissive, minimal) is why nginx-unprivileged
	// wins over distroless-but-personal-repo busybox and the runs-as-root
	// SWS-scratch — the PR records the alternatives weighed. Like DefaultImage,
	// this is a digest-pinned Go constant Renovate does NOT auto-update: bump it
	// deliberately, then run `make verify-image-pins` to confirm the new digest
	// is a multi-arch index.
	StaticServerImage = "nginxinc/nginx-unprivileged:1.30-alpine-slim@sha256:bcb4860e2d7877cf140e4c945f5f9cb304ccb5efbe1dd4fa606a2206142241bf"

	// staticServeRoot is where StaticServerImage serves from; the generated
	// Dockerfile COPYs the build output here.
	staticServeRoot = "/usr/share/nginx/html/"

	// dockerfileLocalName is BOTH the buildctl --local mount name and the
	// dockerfilekey opt value: together they make BuildKit's dockerfile
	// frontend read the Dockerfile from the local mount (forceLocalDockerfile)
	// while the git URL stays the COPY context — the only mechanism that works
	// for a remote git context (verified against moby/buildkit v0.30.0 source +
	// an empirical build). dockerfilekey is an undocumented public opt: re-verify
	// it if DefaultImage is ever bumped off v0.30.x.
	dockerfileLocalName = "dockerfile"
	dockerfileMountPath = "/orkano-dockerfile"
	sourceMountPath     = "/workspace/source"
)

// Invocation is the BuildKit invocation composed from a Build's snapshot of
// the App's source and strategy: which git context to fetch, which Dockerfile
// within it to build, and where to push. Compose is a pure function of the
// Build — no cluster, no I/O — so the five archetype permutations are a table
// test, and the dispatcher's source/strategy snapshot is the only input.
type Invocation struct {
	// ContextURL is the BuildKit git context: the repo at the pinned commit,
	// scoped to source.subPath when set (#commit or #commit:subPath). Pinning
	// the commit rather than the ref keeps the build record honest — the ref
	// is metadata, the commit is what actually built.
	ContextURL string

	// LocalSource is set for ZIP sources and for Nixpacks, both of which need a
	// materialized checkout. Render fills this volume with exactly one immutable
	// source before BuildKit reads it.
	LocalSource *LocalSource

	// DockerfilePath is the Dockerfile to build, relative to the context root
	// (already subPath-scoped). Empty for the Static strategy, whose
	// Dockerfile is generated rather than read from the repo.
	DockerfilePath string

	// GeneratedDockerfile is the COPY-only Dockerfile composed for a Static
	// build (the repo has none); Render injects it into the build pod. Empty
	// for the Dockerfile strategy, which reads its Dockerfile from the repo.
	GeneratedDockerfile string

	// ImageRef is the push target on the in-cluster registry, tagged with the
	// commit; the Build controller resolves the digest after the push.
	ImageRef string

	// NixpacksConfigPath enables the default-off Nixpacks generator. Empty still
	// means Nixpacks when Nixpacks is true; the field itself is optional config.
	Nixpacks           bool
	NixpacksConfigPath string
}

type LocalSource struct {
	GitURL        string
	Commit        string
	ArchiveDigest string
	AppName       string
	SubPath       string
}

// Compose builds the BuildKit invocation for one Build. It trusts CRD
// admission for well-formed inputs — the repo/commit/appName patterns and the
// strategy/members CEL rule — exactly as the rest of the template does, and it
// trusts gitBaseURL the same way: it is operator config (--git-base-url, which
// the operator validates to end in "/"), never CR input, so Compose neither
// re-validates nor normalizes it. An empty gitBaseURL falls back to
// DefaultGitBaseURL, so callers — and the existing tests — that configure no
// override keep the github.com behaviour.
func Compose(build *orkanov1alpha1.Build, gitBaseURL string) (Invocation, error) {
	if build == nil {
		return Invocation{}, errors.New("compose build invocation: Build is required")
	}
	src := build.Spec.Source
	base := gitBaseURL
	if base == "" {
		base = DefaultGitBaseURL
	}
	var gitURL string
	sourceCount := 0
	if src.GitHub != nil {
		sourceCount++
		gitURL = base + src.GitHub.Repo + ".git"
	}
	if src.Git != nil {
		sourceCount++
		gitURL = src.Git.URL
	}
	if src.Upload != nil {
		sourceCount++
		if strings.TrimPrefix(src.Upload.Digest, "sha256:") != build.Spec.Commit {
			return Invocation{}, errors.New("compose build invocation: upload digest does not match Build commit")
		}
	}
	if sourceCount != 1 {
		return Invocation{}, errors.New("compose build invocation: exactly one source is required")
	}
	ctxURL := ""
	if gitURL != "" {
		ctxURL = gitURL + "#" + build.Spec.Commit
	}
	// Trim slashes the CRD pattern allows but BuildKit's git fetcher does not
	// expect: a leading or trailing "/" malforms the subdir fragment, and a
	// bare "/" means the repo root (no subdir at all). Compose owns producing
	// the one canonical context URL.
	if sub := strings.Trim(src.SubPath, "/"); sub != "" {
		if ctxURL != "" {
			ctxURL += ":" + sub
		}
	}
	inv := Invocation{
		ContextURL: ctxURL,
		ImageRef:   RegistryHost + "/" + build.Spec.AppName + ":" + buildImageTag(build),
	}
	needsLocal := src.Upload != nil || build.Spec.Strategy.Strategy == orkanov1alpha1.StrategyNixpacks
	if needsLocal {
		inv.ContextURL = ""
		inv.LocalSource = &LocalSource{GitURL: gitURL, Commit: build.Spec.Commit, AppName: build.Spec.AppName, SubPath: src.SubPath}
		if src.Upload != nil {
			inv.LocalSource.ArchiveDigest = src.Upload.Digest
		}
	}
	switch build.Spec.Strategy.Strategy {
	case orkanov1alpha1.StrategyDockerfile:
		if build.Spec.Strategy.Static != nil || build.Spec.Strategy.Nixpacks != nil {
			return Invocation{}, errors.New("compose build invocation: Dockerfile strategy has mismatched members")
		}
		inv.DockerfilePath = DefaultDockerfile
		if df := build.Spec.Strategy.Dockerfile; df != nil && df.Path != "" {
			inv.DockerfilePath = df.Path
		}
	case orkanov1alpha1.StrategyStatic:
		// No Dockerfile in the repo: generate a COPY-only one and let Render
		// inject it. The CEL rule guarantees static is set for this strategy.
		if s := build.Spec.Strategy.Static; s != nil && build.Spec.Strategy.Dockerfile == nil && build.Spec.Strategy.Nixpacks == nil {
			inv.GeneratedDockerfile = staticDockerfile(s.Dir)
		} else {
			return Invocation{}, errors.New("compose build invocation: Static strategy requires only the static member")
		}
	case orkanov1alpha1.StrategyNixpacks:
		if n := build.Spec.Strategy.Nixpacks; n != nil && build.Spec.Strategy.Dockerfile == nil && build.Spec.Strategy.Static == nil {
			inv.Nixpacks = true
			inv.NixpacksConfigPath = n.ConfigPath
			inv.DockerfilePath = ".nixpacks/Dockerfile"
		} else {
			return Invocation{}, errors.New("compose build invocation: Nixpacks strategy requires only the nixpacks member")
		}
	default:
		return Invocation{}, fmt.Errorf("compose build invocation: unsupported strategy %q", build.Spec.Strategy.Strategy)
	}
	return inv, nil
}

// buildImageTag gives every immutable Build its own mutable registry tag.
// Using only the source revision lets two manual attempts for the same commit
// race: each controller could resolve the digest pushed by the other Job. The
// readable revision prefix plus a full SHA-256 of the Build identity keeps the
// exact tag deterministic for retries while isolating concurrent attempts.
func buildImageTag(build *orkanov1alpha1.Build) string {
	identity := build.Namespace + "/" + build.Name
	if build.Name == "" {
		// Compose's pure unit callers sometimes omit metadata; real admitted
		// Builds always have a name. Keep those callers deterministic without
		// weakening the production identity.
		identity = build.Spec.AppName + "/" + build.Spec.Commit
	}
	sum := sha256.Sum256([]byte(identity))
	revision := build.Spec.Commit
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return revision + "-" + hex.EncodeToString(sum[:])
}

// staticDockerfile is the COPY-only Dockerfile for a Static build: serve the
// build output from StaticServerImage. dir is CRD-validated (subPath's pattern,
// no newline to inject an instruction), trailing slash trimmed to one form.
func staticDockerfile(dir string) string {
	return "FROM " + StaticServerImage + "\n" +
		"COPY " + strings.TrimRight(dir, "/") + "/ " + staticServeRoot + "\n"
}

// Options carries the per-build inputs the template does not derive itself.
type Options struct {
	// ContextURL is the BuildKit git context (https://…#ref or #ref:subdir).
	// Compose builds it from the Build's source; the template treats it as
	// opaque.
	ContextURL string

	LocalSource *LocalSource

	// DockerfilePath is the Dockerfile within the context to build; empty
	// means BuildKit's default ("Dockerfile" at the context root), so the
	// --opt=filename flag is omitted entirely.
	DockerfilePath string

	// GeneratedDockerfile, when set, switches Render to Static mode: it injects
	// this Dockerfile via an init container + local mount instead of reading
	// one from the repo.
	GeneratedDockerfile string

	// ImageRef is the push target on the in-cluster registry; the Build
	// controller resolves the digest after the push.
	ImageRef string

	// Image overrides the BuildKit image (e.g. an air-gapped mirror);
	// empty means DefaultImage.
	Image string

	SourceFetcherImage string
	GitImage           string
	NixpacksImage      string
	Nixpacks           bool
	NixpacksConfigPath string
}

// Render returns the Job that runs one Build. The securityContext deviates
// from restricted in exactly the four ways ADR-0012 enumerates; everything
// else compensates: no ServiceAccount token, hard resource and time limits,
// backoffLimit 0, and the orkano-builds lockdown keyed on the pod label.
func Render(build *orkanov1alpha1.Build, opts Options) (*batchv1.Job, error) {
	if build.Name == "" || build.Namespace == "" {
		return nil, errors.New("rendering build Job: Build has no name or namespace")
	}
	if (opts.ContextURL == "") == (opts.LocalSource == nil) || opts.ImageRef == "" {
		return nil, fmt.Errorf("rendering build Job for %q: exactly one source context and ImageRef are required", build.Name)
	}
	if opts.Nixpacks && opts.LocalSource == nil {
		return nil, fmt.Errorf("rendering build Job for %q: Nixpacks requires a local source", build.Name)
	}
	if opts.LocalSource != nil && opts.LocalSource.ArchiveDigest != "" && opts.SourceFetcherImage == "" {
		return nil, fmt.Errorf("rendering build Job for %q: SourceFetcherImage is required for ZIP sources", build.Name)
	}
	image := opts.Image
	if image == "" {
		image = DefaultImage
	}
	timeout := int64(build.Spec.TimeoutSeconds)
	if timeout == 0 {
		// The CRD defaults timeoutSeconds server-side; this guard keeps a
		// zero-value Build from rendering activeDeadlineSeconds: 0, which
		// would deadline the Job instantly.
		timeout = DefaultTimeoutSeconds
	}

	args := []string{"build", "--frontend=dockerfile.v0"}
	volumeMounts := []corev1.VolumeMount{
		{Name: "buildkitd", MountPath: "/home/user/.local/share/buildkit"},
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "registry-ca", MountPath: caMountPath, ReadOnly: true},
		{Name: "buildkit-config", MountPath: configMountPath, ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: "buildkitd", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "registry-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: CAConfigMapName},
		}}},
		{Name: "buildkit-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configConfigMapName},
		}}},
	}
	var initContainers []corev1.Container
	localContextPath := ""
	if opts.LocalSource == nil {
		args = append(args, "--opt=context="+opts.ContextURL)
	} else {
		contextPath := sourceMountPath
		if sub := strings.Trim(opts.LocalSource.SubPath, "/"); sub != "" {
			contextPath += "/" + sub
		}
		localContextPath = contextPath
		args = append(args, "--local=context="+contextPath)
		volumes = append(volumes, corev1.Volume{Name: "source", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(300<<20, resource.BinarySI)}}})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "source", MountPath: sourceMountPath, ReadOnly: true})
		gitImage := opts.GitImage
		if gitImage == "" {
			gitImage = DefaultGitImage
		}
		if opts.LocalSource.ArchiveDigest != "" {
			initContainers = append(initContainers, archiveInitContainer(opts.SourceFetcherImage, opts.LocalSource))
		} else {
			initContainers = append(initContainers, gitInitContainer(gitImage, opts.LocalSource))
		}
		if opts.Nixpacks {
			nixpacksImage := opts.NixpacksImage
			if nixpacksImage == "" {
				nixpacksImage = DefaultNixpacksImage
			}
			initContainers = append(initContainers, nixpacksInitContainer(nixpacksImage, contextPath, opts.NixpacksConfigPath))
		}
	}

	switch {
	case opts.GeneratedDockerfile != "":
		// Static: read the injected Dockerfile from the local mount while the
		// git URL stays the COPY context (dockerfilekey = forceLocalDockerfile).
		// An init container writes it there — the operator can't create a
		// ConfigMap in orkano-builds, so the content rides in the Job spec.
		args = append(args,
			"--local=dockerfile="+dockerfileMountPath,
			"--opt=dockerfilekey="+dockerfileLocalName,
			"--opt=filename="+DefaultDockerfile,
		)
		volumes = append(volumes, corev1.Volume{Name: dockerfileLocalName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: dockerfileLocalName, MountPath: dockerfileMountPath, ReadOnly: true})
		initContainers = append(initContainers, dockerfileInitContainer(image, opts.GeneratedDockerfile))
	case opts.DockerfilePath != "":
		if localContextPath != "" {
			args = append(args, "--local=dockerfile="+localContextPath)
		}
		args = append(args, "--opt=filename="+opts.DockerfilePath)
	}
	args = append(args, "--output=type=image,name="+opts.ImageRef+",push=true")

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobName(build.Name),
			Namespace: Namespace,
			Labels:    map[string]string{podLabelKey: podLabelValue},
			Annotations: map[string]string{
				AnnotationBuildName:      build.Name,
				AnnotationBuildNamespace: build.Namespace,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To(int32(0)),
			ActiveDeadlineSeconds: ptr.To(timeout),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{podLabelKey: podLabelValue},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           serviceAccountName,
					AutomountServiceAccountToken: ptr.To(false),
					InitContainers:               initContainers,
					Containers: []corev1.Container{{
						Name:    "buildkit",
						Image:   image,
						Command: []string{"buildctl-daemonless.sh"},
						Args:    args,
						Env: []corev1.EnvVar{{
							Name:  "BUILDKITD_FLAGS",
							Value: "--oci-worker-no-process-sandbox --config=" + configMountPath + "/buildkitd.toml",
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("4Gi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:    ptr.To(int64(1000)),
							RunAsGroup:   ptr.To(int64(1000)),
							RunAsNonRoot: ptr.To(true),
							// newuidmap/newgidmap are file-capability binaries:
							// NoNewPrivs or a fully dropped bounding set fails
							// their exec with EPERM (F2 deviations 2+3).
							AllowPrivilegeEscalation: ptr.To(true),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
								Add:  []corev1.Capability{"SETUID", "SETGID"},
							},
							// The cri default profile denies mount(2) silently;
							// this profile re-grants userns + mount only (F2
							// deviation 4). SeccompProfile stays nil on purpose:
							// RuntimeDefault blocks rootlesskit's
							// clone(CLONE_NEWUSER) (F2 deviation 1).
							AppArmorProfile: &corev1.AppArmorProfile{
								Type:             corev1.AppArmorProfileTypeLocalhost,
								LocalhostProfile: ptr.To(appArmorProfileName),
							},
						},
						VolumeMounts: volumeMounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}, nil
}

// dockerfileInitContainer writes the generated Dockerfile into the shared
// emptyDir the buildkit container reads via --local. It reuses the build image
// (Alpine-based: provides sh + printf) but needs none of that container's
// rootless deviations — just to write one file — so it runs at a
// restricted-grade securityContext. The content rides in an env var and is
// written with printf %s, so a Dockerfile value is data, never shell.
func dockerfileInitContainer(image, dockerfile string) corev1.Container {
	return corev1.Container{
		Name:    "render-dockerfile",
		Image:   image,
		Command: []string{"sh", "-c", `printf '%s' "$ORKANO_DOCKERFILE" > ` + dockerfileMountPath + "/" + DefaultDockerfile},
		Env:     []corev1.EnvVar{{Name: "ORKANO_DOCKERFILE", Value: dockerfile}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dockerfileLocalName, MountPath: dockerfileMountPath},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To(int64(1000)),
			RunAsGroup:               ptr.To(int64(1000)),
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}

func archiveInitContainer(image string, source *LocalSource) corev1.Container {
	return corev1.Container{
		Name:  "fetch-source-archive",
		Image: image,
		Args: []string{
			"source-fetch",
			"--app=" + source.AppName,
			"--digest=" + source.ArchiveDigest,
			"--destination=" + sourceMountPath,
			"--registry-url=https://" + RegistryHost,
			"--ca-file=" + caMountPath + "/ca.crt",
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "source", MountPath: sourceMountPath},
			{Name: "tmp", MountPath: "/tmp"},
			{Name: "registry-ca", MountPath: caMountPath, ReadOnly: true},
		},
		Resources:       initResources("100m", "128Mi"),
		SecurityContext: restrictedInitSecurityContext(),
	}
}

func gitInitContainer(image string, source *LocalSource) corev1.Container {
	return corev1.Container{
		Name:    "checkout-source",
		Image:   image,
		Command: []string{"/bin/sh", "-ec"},
		Args: []string{`umask 022
git init "$ORKANO_SOURCE_DIR"
git -C "$ORKANO_SOURCE_DIR" remote add origin "$ORKANO_GIT_URL"
git -C "$ORKANO_SOURCE_DIR" -c protocol.file.allow=never fetch --depth=1 origin "$ORKANO_COMMIT"
test "$(git -C "$ORKANO_SOURCE_DIR" rev-parse FETCH_HEAD)" = "$ORKANO_COMMIT"
git -C "$ORKANO_SOURCE_DIR" checkout --detach FETCH_HEAD
test "$(git -C "$ORKANO_SOURCE_DIR" rev-parse HEAD)" = "$ORKANO_COMMIT"
rm -rf "$ORKANO_SOURCE_DIR/.git"`},
		Env: []corev1.EnvVar{
			{Name: "ORKANO_SOURCE_DIR", Value: sourceMountPath},
			{Name: "ORKANO_GIT_URL", Value: source.GitURL},
			{Name: "ORKANO_COMMIT", Value: source.Commit},
		},
		VolumeMounts:    []corev1.VolumeMount{{Name: "source", MountPath: sourceMountPath}},
		Resources:       initResources("100m", "128Mi"),
		SecurityContext: restrictedInitSecurityContext(),
	}
}

func nixpacksInitContainer(image, sourceDir, configPath string) corev1.Container {
	return corev1.Container{
		Name:    "generate-nixpacks",
		Image:   image,
		Command: []string{"/bin/bash", "-ec"},
		Args: []string{`set -euo pipefail
case "$(uname -m)" in
  x86_64) asset=x86_64-unknown-linux-musl; expected=0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6 ;;
  aarch64|arm64) asset=aarch64-unknown-linux-musl; expected=912bd02dd2bb6f9c3a9ed965fe8a68b4aa318dc7a2546e2eca6f2806a894ba39 ;;
  *) echo "unsupported Nixpacks architecture: $(uname -m)" >&2; exit 1 ;;
esac
archive=/tmp/nixpacks.tar.gz
curl --proto '=https' --tlsv1.2 -fsSL "https://github.com/railwayapp/nixpacks/releases/download/v1.41.0/nixpacks-v1.41.0-${asset}.tar.gz" -o "$archive"
printf '%s  %s\n' "$expected" "$archive" | sha256sum -c -
tar -xzf "$archive" -C /tmp
set -- /tmp/nixpacks build "$ORKANO_SOURCE_DIR" --out "$ORKANO_SOURCE_DIR"
if [ -n "$ORKANO_NIXPACKS_CONFIG" ]; then set -- "$@" --config "$ORKANO_NIXPACKS_CONFIG"; fi
"$@"
test -s "$ORKANO_SOURCE_DIR/.nixpacks/Dockerfile"`},
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: "/tmp"},
			{Name: "ORKANO_SOURCE_DIR", Value: sourceDir},
			{Name: "ORKANO_NIXPACKS_CONFIG", Value: configPath},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "source", MountPath: sourceMountPath},
			{Name: "tmp", MountPath: "/tmp"},
		},
		Resources:       initResources("100m", "256Mi"),
		SecurityContext: restrictedInitSecurityContext(),
	}
}

func initResources(cpu, memory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		},
	}
}

func restrictedInitSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                ptr.To(int64(1000)),
		RunAsGroup:               ptr.To(int64(1000)),
		RunAsNonRoot:             ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// JobName caps at 63 characters: the Job controller stamps the name onto
// pods as the batch.kubernetes.io/job-name label, and label values cannot
// exceed that. Longer Build names keep a unique tail hashed from the full
// name; the trim keeps the truncation point DNS-legal. Exported because the
// Build controller derives the same name when it has no Job in hand (the
// finalizer's cancel/cleanup path).
func JobName(buildName string) string {
	if len(buildName) <= 63 {
		return buildName
	}
	sum := sha256.Sum256([]byte(buildName))
	return strings.TrimRight(buildName[:54], "-.") + "-" + hex.EncodeToString(sum[:4])
}
