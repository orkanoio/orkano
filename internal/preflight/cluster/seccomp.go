package cluster

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/orkanoio/orkano/api/check"
)

const seccompCanaryCommand = `if grep -Eq '^Seccomp:[[:space:]]*0$' /proc/self/status; then exit 0; fi; if grep -Eq '^Seccomp:[[:space:]]*[1-9][0-9]*$' /proc/self/status; then exit 42; fi; exit 43`

// seccompDefaultDisabledCheck tests the effective behavior that ADR-0012 needs
// rather than trusting a kubelet flag: BuildKit intentionally omits seccomp,
// so its process must observe Seccomp: 0 on every node it may reach.
func seccompDefaultDisabledCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDSeccompDefaultDisabled,
		Severity: check.SeverityCritical,
		Summary:  "nil-seccomp build canaries are unconfined on every schedulable Linux build node",
		Remediation: "disable kubelet seccompDefault (or any equivalent defaulting that applies RuntimeDefault to fieldless pods) " +
			"on every Linux build node; ADR-0012's rootless BuildKit configuration requires nil seccomp to remain unconfined. " +
			"Run the preflight with an identity that can list nodes, create and get pods, and delete its labeled scratch namespaces",
		Probe: func(ctx context.Context) (check.Result, error) {
			nodes, err := eligibleBuildNodes(ctx, opt.Client)
			if err != nil {
				return check.Result{}, err
			}
			if len(nodes) == 0 {
				return check.Result{
					Status:  check.StatusFail,
					Message: "no Ready, schedulable Linux node can run a nil-seccomp build canary",
				}, nil
			}

			return withScratchNamespace(ctx, opt.Client, "orkano-preflight-seccomp-", "baseline", func(ctx context.Context, namespace string) (check.Result, error) {
				probeCtx, cancel := context.WithTimeout(ctx, liveProbeWaitBudget)
				defer cancel()

				canaries, err := createNodeCanaries(probeCtx, opt.Client, namespace, nodes, func(buildNode) *corev1.Pod {
					pod := restrictedCanaryPod(namespace, "canary-", "seccomp", []string{"sh", "-c", seccompCanaryCommand}, 60)
					// The test is intentionally fieldless at both scopes. Explicit
					// Unconfined would itself violate baseline PSA and would not
					// reproduce the Build Job's contract.
					pod.Spec.SecurityContext.SeccompProfile = nil
					pod.Spec.Containers[0].SecurityContext.SeccompProfile = nil
					return pod
				})
				if err != nil {
					return check.Result{}, err
				}

				var failures, indeterminate []string
				for _, canary := range canaries {
					pod, err := waitForPodTerminal(probeCtx, opt.Client, namespace, canary.pod.Name)
					if err != nil {
						indeterminate = append(indeterminate, fmt.Sprintf("%s: %v", canary.node.name, err))
						continue
					}

					exitCode, ok := terminalExitCode(pod)
					switch {
					case ok && pod.Status.Phase == corev1.PodSucceeded && exitCode == 0:
						continue
					case ok && exitCode == canaryExitExpectedBlocked:
						failures = append(failures, canary.node.name)
					case ok && exitCode == canaryExitInvalid:
						indeterminate = append(indeterminate, fmt.Sprintf("%s: canary could not parse /proc/self/status", canary.node.name))
					default:
						indeterminate = append(indeterminate, fmt.Sprintf("%s: canary ended with %s", canary.node.name, podDetail(pod)))
					}
				}

				if len(indeterminate) > 0 {
					message := "nil-seccomp capability could not be determined on " + strings.Join(indeterminate, "; ")
					if len(failures) > 0 {
						message += "; nodes with nonzero Seccomp mode: " + strings.Join(failures, ", ")
					}
					return check.Result{}, fmt.Errorf("%s", message)
				}
				if len(failures) > 0 {
					return check.Result{
						Status: check.StatusFail,
						Message: "nil-seccomp canaries observed a nonzero Seccomp mode on " + strings.Join(failures, ", ") +
							" — kubelet seccompDefault or equivalent runtime defaulting would break ADR-0012 rootless builds",
					}, nil
				}
				return check.Result{
					Status:  check.StatusPass,
					Message: fmt.Sprintf("nil-seccomp canary observed Seccomp: 0 on %d Linux build node(s): %s", len(nodes), nodeNames(nodes)),
				}, nil
			})
		},
	}
}
