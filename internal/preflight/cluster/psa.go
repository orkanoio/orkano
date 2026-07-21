package cluster

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"

	"github.com/orkanoio/orkano/api/check"
)

// podSecurityAdmissionEnforcedCheck proves that namespace PSA labels are an
// active admission boundary. A privileged pod is the detector, but a scheduling
// gate guarantees it cannot execute if a cluster has PSA disabled and accepts
// the object.
func podSecurityAdmissionEnforcedCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDPodSecurityAdmissionEnforced,
		Severity: check.SeverityCritical,
		Summary:  "Pod Security Admission enforces a restricted namespace label",
		Remediation: "enable the PodSecurity admission plugin (or the managed-cluster equivalent) and ensure namespace " +
			"labels pod-security.kubernetes.io/enforce=restricted are honored before installing Orkano. Run the preflight with an " +
			"identity that can create, get and delete its labeled scratch namespaces and create pods",
		Probe: func(ctx context.Context) (check.Result, error) {
			return withScratchNamespace(ctx, opt.Client, "orkano-preflight-psa-", "restricted", func(ctx context.Context, namespace string) (check.Result, error) {
				pod := restrictedCanaryPod(namespace, "privileged-", "psa", []string{"sh", "-c", "exit 0"}, 60)
				pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: "orkano.io/preflight-psa"}}
				pod.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true)

				err := opt.Client.Create(ctx, pod)
				switch {
				case err == nil:
					return check.Result{
						Status:  check.StatusFail,
						Message: "a privileged pod was admitted in a namespace enforcing restricted; Pod Security Admission is not active",
					}, nil
				case apierrors.IsForbidden(err) && strings.Contains(err.Error(), "PodSecurity"):
					return check.Result{
						Status:  check.StatusPass,
						Message: "a scheduling-gated privileged canary was rejected by PodSecurity in a restricted namespace",
					}, nil
				default:
					return check.Result{}, fmt.Errorf("create privileged PSA canary: %w", err)
				}
			})
		},
	}
}
