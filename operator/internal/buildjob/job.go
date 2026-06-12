// Package buildjob renders the Job that runs one Build as rootless BuildKit.
//
// The shape is spike attempt F2 verbatim — the minimal configuration PSA
// baseline admits with zero warnings (ADR-0012) — plus the two product
// deltas: the build context is a git URL fetched over the egress
// allowlist's 443 rule, and the push target is the TLS registry, trusted
// through the cluster-internal CA (registry.insecure never ships).
// Rendering is pure: the Build controller owns creation, status, and
// cleanup. A golden copy of the rendered Job is pinned at
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
	DefaultImage = "moby/buildkit:v0.30.0-rootless@sha256:d76eb1caecac5733ef7553c1e90a1b21f1bb218cd1142d3553de0747b4a14ba9"

	// podLabelKey/Value is the NetworkPolicy contract (config/netpol/): a
	// pod without this label gets no network in orkano-builds, fail closed.
	podLabelKey   = "app.kubernetes.io/name"
	podLabelValue = "orkano-build"

	// caConfigMapName is published at install from the registry TLS Secret's
	// ca.crt (M1.5 contract); the smoke's TLS probe uses the same projection.
	caConfigMapName = "orkano-registry-ca"
	caMountPath     = "/orkano-registry-ca"

	// configConfigMapName carries buildkitd.toml (config/buildkit/), which
	// points BuildKit's registry client at the projected CA; a test pins
	// that manifest against these constants.
	configConfigMapName = "orkano-buildkit-config"
	configMountPath     = "/orkano-buildkit-config"

	appArmorProfileName = "orkano-buildkit"

	defaultTimeoutSeconds = 900
)

// Options carries the per-build inputs the template does not derive itself.
type Options struct {
	// ContextURL is the BuildKit git context (https://…#ref or #ref:subdir).
	// Composing it from the Build's source is the invocation-composition
	// task; the template treats it as opaque.
	ContextURL string

	// ImageRef is the push target on the in-cluster registry; the Build
	// controller resolves the digest after the push.
	ImageRef string

	// Image overrides the BuildKit image (e.g. an air-gapped mirror);
	// empty means DefaultImage.
	Image string
}

// Render returns the Job that runs one Build. The securityContext deviates
// from restricted in exactly the four ways ADR-0012 enumerates; everything
// else compensates: no ServiceAccount token, hard resource and time limits,
// backoffLimit 0, and the orkano-builds lockdown keyed on the pod label.
func Render(build *orkanov1alpha1.Build, opts Options) (*batchv1.Job, error) {
	if build.Name == "" {
		return nil, errors.New("rendering build Job: Build has no name")
	}
	if opts.ContextURL == "" || opts.ImageRef == "" {
		return nil, fmt.Errorf("rendering build Job for %q: ContextURL and ImageRef are required", build.Name)
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
		timeout = defaultTimeoutSeconds
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(build.Name),
			Namespace: Namespace,
			Labels:    map[string]string{podLabelKey: podLabelValue},
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
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{
						Name:    "buildkit",
						Image:   image,
						Command: []string{"buildctl-daemonless.sh"},
						Args: []string{
							"build",
							"--frontend=dockerfile.v0",
							"--opt=context=" + opts.ContextURL,
							"--output=type=image,name=" + opts.ImageRef + ",push=true",
						},
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
						VolumeMounts: []corev1.VolumeMount{
							{Name: "buildkitd", MountPath: "/home/user/.local/share/buildkit"},
							{Name: "tmp", MountPath: "/tmp"},
							{Name: "registry-ca", MountPath: caMountPath, ReadOnly: true},
							{Name: "buildkit-config", MountPath: configMountPath, ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "buildkitd", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "registry-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: caConfigMapName},
						}}},
						{Name: "buildkit-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: configConfigMapName},
						}}},
					},
				},
			},
		},
	}, nil
}

// jobName caps at 63 characters: the Job controller stamps the name onto
// pods as the batch.kubernetes.io/job-name label, and label values cannot
// exceed that. Longer Build names keep a unique tail hashed from the full
// name; the trim keeps the truncation point DNS-legal.
func jobName(buildName string) string {
	if len(buildName) <= 63 {
		return buildName
	}
	sum := sha256.Sum256([]byte(buildName))
	return strings.TrimRight(buildName[:54], "-.") + "-" + hex.EncodeToString(sum[:4])
}
