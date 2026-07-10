package cluster

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/nodeprep"
)

const appArmorCanaryCommand = `profile=$(cat /proc/self/attr/current) || exit 43; [ "$profile" = "` + nodeprep.ProfileName + ` (enforce)" ] && exit 0; exit 42`

// appArmorCapableCheck tests every node a normal Linux Build can currently
// reach. The scheduler cannot see Localhost profiles, so a single successful
// placement would otherwise hide a secondary node that fails the first build.
func appArmorCapableCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDAppArmorCapable,
		Severity: check.SeverityCritical,
		Summary:  "every schedulable Linux build node starts an orkano-buildkit AppArmor canary in enforce mode",
		Remediation: "load the orkano-buildkit AppArmor profile in enforce mode on every Linux node that can run builds " +
			"(enable the BYO node-prep DaemonSet when available, or apply the documented manual node preparation); " +
			"clusters without AppArmor-capable build nodes cannot run Orkano builds. Run the preflight with an identity that can list " +
			"nodes and events; create and get pods; and delete its labeled scratch namespaces",
		Probe: func(ctx context.Context) (check.Result, error) {
			nodes, err := eligibleBuildNodes(ctx, opt.Client)
			if err != nil {
				return check.Result{}, err
			}
			if len(nodes) == 0 {
				return check.Result{
					Status:  check.StatusFail,
					Message: "no Ready, schedulable Linux node can run an Orkano build canary",
				}, nil
			}

			return withScratchNamespace(ctx, opt.Client, "orkano-preflight-apparmor-", "baseline", func(ctx context.Context, namespace string) (check.Result, error) {
				probeCtx, cancel := context.WithTimeout(ctx, liveProbeWaitBudget)
				defer cancel()

				canaries, err := createNodeCanaries(probeCtx, opt.Client, namespace, nodes, func(buildNode) *corev1.Pod {
					pod := restrictedCanaryPod(namespace, "canary-", "apparmor", []string{"sh", "-c", appArmorCanaryCommand}, 60)
					pod.Spec.Containers[0].SecurityContext.AppArmorProfile = &corev1.AppArmorProfile{
						Type:             corev1.AppArmorProfileTypeLocalhost,
						LocalhostProfile: ptr.To(nodeprep.ProfileName),
					}
					return pod
				})
				if err != nil {
					return check.Result{}, err
				}

				var failures, indeterminate []string
				for _, canary := range canaries {
					pod, err := waitForPodTerminal(probeCtx, opt.Client, namespace, canary.pod.Name)
					if err != nil {
						if ctx.Err() != nil {
							return check.Result{}, fmt.Errorf("wait for AppArmor canary on %s: %w", canary.node.name, err)
						}
						evidence, evidenceErr := appArmorFailureEvidence(opt.Client, namespace, canary.pod.Name)
						if evidenceErr != nil {
							indeterminate = append(indeterminate, fmt.Sprintf("%s: %v (and could not inspect AppArmor evidence: %v)", canary.node.name, err, evidenceErr))
							continue
						}
						if evidence != "" {
							return check.Result{
								Status:  check.StatusFail,
								Message: fmt.Sprintf("orkano-buildkit AppArmor canary failed on %s: %s", canary.node.name, evidence),
							}, nil
						}
						indeterminate = append(indeterminate, fmt.Sprintf("%s: %v", canary.node.name, err))
						continue
					}

					exitCode, ok := terminalExitCode(pod)
					switch {
					case ok && pod.Status.Phase == corev1.PodSucceeded && exitCode == 0:
						continue
					case ok && exitCode == canaryExitExpectedBlocked:
						failures = append(failures, fmt.Sprintf("%s: Localhost profile %q was not applied in enforce mode", canary.node.name, nodeprep.ProfileName))
					default:
						indeterminate = append(indeterminate, fmt.Sprintf("%s: canary ended with %s", canary.node.name, podDetail(pod)))
					}
				}

				if len(indeterminate) > 0 {
					message := "AppArmor capability could not be determined on " + strings.Join(indeterminate, "; ")
					if len(failures) > 0 {
						message += "; known failures: " + strings.Join(failures, "; ")
					}
					return check.Result{}, fmt.Errorf("%s", message)
				}
				if len(failures) > 0 {
					return check.Result{
						Status:  check.StatusFail,
						Message: "orkano-buildkit AppArmor canaries failed: " + strings.Join(failures, "; "),
					}, nil
				}
				return check.Result{
					Status:  check.StatusPass,
					Message: fmt.Sprintf("orkano-buildkit AppArmor canary ran in enforce mode on %d Linux build node(s): %s", len(nodes), nodeNames(nodes)),
				}, nil
			})
		},
	}
}
