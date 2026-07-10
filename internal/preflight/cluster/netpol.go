package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
)

const (
	netpolRoleServer  = "netpol-server"
	netpolRoleControl = "netpol-control"
	netpolRoleDeny    = "netpol-deny"
)

type networkOutcome int

const (
	networkOutcomeConnected networkOutcome = iota
	networkOutcomeBlocked
)

// networkPolicyEnforcedCheck proves default-deny egress from every node that
// can run a normal Linux Build. NetworkPolicy changes to existing connections
// are implementation-defined, so every attempt creates fresh client pods.
func networkPolicyEnforcedCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDNetworkPolicyEnforced,
		Severity: check.SeverityCritical,
		Summary:  "the CNI enforces default-deny egress (capability-probed)",
		Remediation: "install or enable a CNI that enforces Kubernetes NetworkPolicy; a policy object alone is not proof — " +
			"the probe's fresh denied TCP canary must be unable to reach an in-namespace server. Run the preflight with an identity " +
			"that can list nodes; create, get and delete its labeled scratch namespaces and pods; and create NetworkPolicies",
		Probe: func(ctx context.Context) (check.Result, error) {
			nodes, err := eligibleBuildNodes(ctx, opt.Client)
			if err != nil {
				return check.Result{}, err
			}
			if len(nodes) == 0 {
				return check.Result{
					Status:  check.StatusFail,
					Message: "no Ready, schedulable Linux node can run a NetworkPolicy canary",
				}, nil
			}

			return withScratchNamespace(ctx, opt.Client, "orkano-preflight-netpol-", "restricted", func(ctx context.Context, namespace string) (check.Result, error) {
				probeCtx, cancel := context.WithTimeout(ctx, liveProbeWaitBudget)
				defer cancel()

				server := restrictedCanaryPod(namespace, "server-", netpolRoleServer, []string{"sh", "-c", "exec httpd -f -p 8080"}, 180)
				pinCanaryToNode(server, nodes[0])
				if err := opt.Client.Create(probeCtx, server); err != nil {
					return check.Result{}, fmt.Errorf("create NetworkPolicy server canary: %w", err)
				}
				server, err := waitForPod(probeCtx, opt.Client, namespace, server.Name, "Running with a pod IP", func(pod *corev1.Pod) bool {
					return (pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "") ||
						pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
				})
				if err != nil {
					return check.Result{}, fmt.Errorf("wait for NetworkPolicy server canary: %w", err)
				}
				if server.Status.Phase != corev1.PodRunning || server.Status.PodIP == "" {
					return check.Result{}, fmt.Errorf("NetworkPolicy server canary did not become usable: %s", podDetail(server))
				}
				baseline, err := createNetworkClientCanaries(probeCtx, opt.Client, namespace, "baseline-deny-", netpolRoleDeny, server.Status.PodIP, nodes)
				if err != nil {
					return check.Result{}, err
				}
				if err := requireNetworkCanariesConnected(probeCtx, opt.Client, namespace, "pre-policy deny-label", server.Status.PodIP, baseline); err != nil {
					return check.Result{}, err
				}

				for _, policy := range networkPolicies(namespace) {
					if err := opt.Client.Create(probeCtx, policy); err != nil {
						return check.Result{}, fmt.Errorf("create NetworkPolicy %s: %w", policy.Name, err)
					}
				}

				attempts := 0
				lastAllControlsConnected := false
				lastDenyConnectedNodes := []string(nil)
				for {
					attempts++
					control, err := createNetworkClientCanaries(probeCtx, opt.Client, namespace, "control-", netpolRoleControl, server.Status.PodIP, nodes)
					if err != nil {
						return check.Result{}, err
					}
					deny, err := createNetworkClientCanaries(probeCtx, opt.Client, namespace, "deny-", netpolRoleDeny, server.Status.PodIP, nodes)
					if err != nil {
						return check.Result{}, err
					}

					allControlsConnected, err := networkCanariesConnected(probeCtx, opt.Client, namespace, "allowed", control)
					if err != nil {
						return check.Result{}, err
					}
					denyConnectedNodes, allDeniedBlocked, err := networkCanariesBlocked(probeCtx, opt.Client, namespace, deny)
					if err != nil {
						return check.Result{}, err
					}
					if allControlsConnected {
						confirmation, err := createNetworkClientCanaries(probeCtx, opt.Client, namespace, "confirm-control-", netpolRoleControl, server.Status.PodIP, nodes)
						if err != nil {
							return check.Result{}, err
						}
						allControlsConnected, err = networkCanariesConnected(probeCtx, opt.Client, namespace, "post-deny allowed", confirmation)
						if err != nil {
							return check.Result{}, err
						}
					}

					lastAllControlsConnected = allControlsConnected
					lastDenyConnectedNodes = denyConnectedNodes
					if allControlsConnected && allDeniedBlocked {
						return check.Result{
							Status:  check.StatusPass,
							Message: fmt.Sprintf("fresh denied TCP canaries were blocked from all %d Linux build node(s) while pre- and post-deny allowlisted controls reached %s:8080 (after %d attempt(s))", len(nodes), server.Status.PodIP, attempts),
						}, nil
					}

					select {
					case <-probeCtx.Done():
						if ctx.Err() != nil {
							return check.Result{}, fmt.Errorf("NetworkPolicy probe cancelled: %w", ctx.Err())
						}
						if lastAllControlsConnected && len(lastDenyConnectedNodes) > 0 {
							return check.Result{
								Status:  check.StatusFail,
								Message: fmt.Sprintf("egress-denied canaries on %s kept reaching %s:8080 after %d fresh attempt(s) while every allowlisted control connected — the CNI is not enforcing default-deny egress", strings.Join(lastDenyConnectedNodes, ", "), server.Status.PodIP, attempts),
							}, nil
						}
						return check.Result{}, fmt.Errorf("the latest NetworkPolicy canary batch did not prove a healthy allowlisted control and blocked denied source from every Linux build node before %w", probeCtx.Err())
					case <-time.After(liveProbePollInterval):
					}
				}
			})
		},
	}
}

func networkPolicies(namespace string) []*networkingv1.NetworkPolicy {
	protocol := corev1.ProtocolTCP
	port := intstr.FromInt(8080)
	return []*networkingv1.NetworkPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "default-deny-egress"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "allow-control-egress"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{probeRoleLabel: netpolRoleControl}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{probeRoleLabel: netpolRoleServer}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &port}},
				}},
			},
		},
	}
}

func createNetworkClientCanaries(ctx context.Context, c client.Client, namespace, generateName, role, serverIP string, nodes []buildNode) ([]nodeCanary, error) {
	command := []string{"sh", "-c", `nc -z -w 5 "$ORKANO_PROBE_TARGET" 8080; case $? in 0) exit 0;; 1) exit 42;; *) exit 43;; esac`}
	return createNodeCanaries(ctx, c, namespace, nodes, func(buildNode) *corev1.Pod {
		pod := restrictedCanaryPod(namespace, generateName, role, command, 30)
		pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "ORKANO_PROBE_TARGET", Value: serverIP}}
		return pod
	})
}

func requireNetworkCanariesConnected(ctx context.Context, c client.Client, namespace, role, serverIP string, canaries []nodeCanary) error {
	allConnected, err := networkCanariesConnected(ctx, c, namespace, role, canaries)
	if err != nil {
		return err
	}
	if !allConnected {
		return fmt.Errorf("%s canary could not reach %s:8080 from every Linux build node — another policy or network control is already blocking a source, so NetworkPolicy enforcement cannot be attributed", role, serverIP)
	}
	return nil
}

func networkCanariesConnected(ctx context.Context, c client.Client, namespace, role string, canaries []nodeCanary) (bool, error) {
	allConnected := true
	for _, canary := range canaries {
		pod, err := waitForPodTerminal(ctx, c, namespace, canary.pod.Name)
		if err != nil {
			return false, fmt.Errorf("wait for %s NetworkPolicy canary on %s: %w", role, canary.node.name, err)
		}
		outcome, err := networkCanaryOutcome(pod)
		if err != nil {
			return false, fmt.Errorf("%s NetworkPolicy canary on %s: %w", role, canary.node.name, err)
		}
		if outcome != networkOutcomeConnected {
			allConnected = false
		}
	}
	return allConnected, nil
}

func networkCanariesBlocked(ctx context.Context, c client.Client, namespace string, canaries []nodeCanary) ([]string, bool, error) {
	connectedNodes := make([]string, 0)
	allBlocked := true
	for _, canary := range canaries {
		pod, err := waitForPodTerminal(ctx, c, namespace, canary.pod.Name)
		if err != nil {
			return nil, false, fmt.Errorf("wait for denied NetworkPolicy canary on %s: %w", canary.node.name, err)
		}
		outcome, err := networkCanaryOutcome(pod)
		if err != nil {
			return nil, false, fmt.Errorf("denied NetworkPolicy canary on %s: %w", canary.node.name, err)
		}
		if outcome == networkOutcomeConnected {
			connectedNodes = append(connectedNodes, canary.node.name)
			allBlocked = false
		}
	}
	return connectedNodes, allBlocked, nil
}

func networkCanaryOutcome(pod *corev1.Pod) (networkOutcome, error) {
	exitCode, ok := terminalExitCode(pod)
	if !ok {
		return 0, fmt.Errorf("no container exit code (%s)", podDetail(pod))
	}
	switch {
	case pod.Status.Phase == corev1.PodSucceeded && exitCode == 0:
		return networkOutcomeConnected, nil
	case exitCode == canaryExitExpectedBlocked:
		return networkOutcomeBlocked, nil
	case exitCode == canaryExitInvalid:
		return 0, fmt.Errorf("canary could not execute the connection test (%s)", podDetail(pod))
	default:
		return 0, fmt.Errorf("unexpected canary exit (%s)", podDetail(pod))
	}
}
