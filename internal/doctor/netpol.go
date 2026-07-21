package doctor

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
)

// IDNetworkPolicyEnforced is PERMANENT — it appears in --json output and CI
// configs.
const IDNetworkPolicyEnforced = "net.networkpolicy-enforced"

// CanaryImage runs the two connectivity canaries. Digest-pinned multi-arch
// INDEX (amd64+arm64), a Go constant like buildjob.DefaultImage — Renovate
// does not bump it; re-resolve the index digest deliberately.
const CanaryImage = "busybox:1.37@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028"

const (
	// registryServiceName is the in-cluster registry Service (orkano-system),
	// the canaries' connection target: its portless VIP:443 is reachable by
	// build-labeled pods and denied to everything else — one target that
	// exercises both the orkano-builds default-deny and the registry ingress
	// allowlist.
	registryServiceName = "orkano-registry"

	// buildNamespace is where the canaries run: orkano-builds carries the
	// default-deny NetworkPolicy keyed on the build-pod label contract.
	buildNamespace = "orkano-builds"

	// buildPodLabel is the label contract from config/netpol/orkano-builds.yaml:
	// pods carrying it get the egress allowlist, pods without it get nothing.
	buildPodLabel = "orkano-build"

	// canaryManagedByValue marks doctor's canary pods so a crashed run's
	// leftovers can be swept by label on the next run.
	canaryManagedByValue = "orkano-doctor"

	// canaryLabelValue is the deny canaries' app label — deliberately
	// matching NO podSelector in config/netpol/, so they get only the
	// namespace default-deny.
	canaryLabelValue = "orkano-doctor-canary"
)

// Package vars so tests can shrink the probe's timing.
var (
	netpolWaitBudget   = 3 * time.Minute
	netpolPollInterval = 2 * time.Second
)

// networkPolicyEnforcedCheck is the live capability probe for the substrate
// assumption every INV-02 control stands on: the CNI actually enforces
// NetworkPolicy. Reading policy objects proves nothing — a CNI without
// enforcement accepts them silently — so the probe attempts the forbidden
// thing with three canaries in orkano-builds:
//
//   - control (build-labeled, -> registry VIP): must CONNECT. The probe's
//     health gate — without it a dead registry masquerades as enforcement.
//   - deny-egress (unlabeled, -> apiserver VIP): must be BLOCKED. Nothing
//     guards the apiserver's ingress, so only the orkano-builds default-deny
//     EGRESS can block it — this leg isolates the INV-02-critical direction.
//     The registry alone cannot: it is guarded on BOTH ends, so a deleted
//     egress policy (or an ingress-only CNI, a documented kube-router bug
//     class) would false-pass a registry-only deny leg.
//   - deny-registry (unlabeled, -> registry VIP): must be BLOCKED — the
//     belt-and-braces leg through both the default-deny and the registry
//     ingress allowlist.
//
// Critical severity — if this fails, the build sandbox has no network
// boundary. The probe creates three short-lived pods (restricted-grade, no
// SA token) and deletes them afterwards; it is safe to re-run and sweeps
// leftovers from crashed runs by label first. NOTE the cost: unlike its
// read-only siblings this schedules real pods (tens of seconds), and a
// --fix run re-probes the whole registry — acceptable while no doctor check
// ships a Fix, revisit if one does.
func networkPolicyEnforcedCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDNetworkPolicyEnforced,
		Severity: check.SeverityCritical,
		Summary:  "the CNI enforces NetworkPolicy (capability-probed, INV-02 substrate)",
		Remediation: "run `kubectl get networkpolicy -n orkano-builds` — if the policies are missing, re-apply config/netpol/; " +
			"if they exist, the CNI is not enforcing them: on the stock k3s install kube-router's netpol controller must be running " +
			"(re-run `orkano init`), on a custom CNI install one that enforces NetworkPolicy",
		Probe: func(ctx context.Context) (check.Result, error) {
			registryVIP, err := serviceVIP(ctx, opt.Client, systemNamespace, registryServiceName)
			if err != nil {
				return check.Result{}, err
			}
			apiVIP, err := serviceVIP(ctx, opt.Client, "default", "kubernetes")
			if err != nil {
				return check.Result{}, err
			}

			if err := sweepCanaries(ctx, opt.Client); err != nil {
				return check.Result{}, err
			}

			stamp := opt.now().Unix()
			control := canaryPod(fmt.Sprintf("orkano-doctor-netpol-control-%d", stamp), buildPodLabel, registryVIP)
			denyRegistry := canaryPod(fmt.Sprintf("orkano-doctor-netpol-deny-registry-%d", stamp), canaryLabelValue, registryVIP)
			denyEgress := canaryPod(fmt.Sprintf("orkano-doctor-netpol-deny-egress-%d", stamp), canaryLabelValue, apiVIP)
			pods := []*corev1.Pod{control, denyRegistry, denyEgress}

			cleanup := func() {
				for _, p := range pods {
					deleteCanary(opt.Client, p)
				}
			}
			for _, p := range pods {
				if err := opt.Client.Create(ctx, p); err != nil {
					cleanup()
					return check.Result{}, fmt.Errorf("create canary pod %s: %w", p.Name, err)
				}
			}
			defer cleanup()

			// One shared ceiling for ALL waits, deliberately: the pods run
			// concurrently on the cluster regardless of our sequential
			// polling, and a doctor run should be bounded by one budget. A
			// slow leg eating the others' window degrades to a probe error,
			// never a wrong verdict.
			wait, cancel := context.WithTimeout(ctx, netpolWaitBudget)
			defer cancel()
			phases := make(map[string]corev1.PodPhase, len(pods))
			for _, p := range pods {
				phase, err := waitCanaryDone(wait, opt.Client, p.Name)
				if err != nil {
					return check.Result{}, fmt.Errorf("wait for canary %s (%s): %w", p.Name, canaryDetail(ctx, opt.Client, p.Name), err)
				}
				phases[p.Name] = phase
			}

			// The control leg is the probe's own health gate: if the ALLOWED
			// path cannot connect, blocked deny canaries prove nothing (the
			// registry may simply be down), so the result is indeterminate.
			if phases[control.Name] != corev1.PodSucceeded {
				return check.Result{}, fmt.Errorf(
					"the allowed control path (build-labeled pod -> registry %s:443) did not connect (%s) — cannot attribute the deny results to policy; "+
						"check the registry's health, and whether the canary image %s is pullable on the nodes (air-gapped or rate-limited installs need it preloaded)",
					registryVIP, canaryDetail(ctx, opt.Client, control.Name), CanaryImage)
			}

			if phases[denyEgress.Name] == corev1.PodSucceeded {
				return check.Result{
					Status: check.StatusFail,
					Message: fmt.Sprintf("an unlabeled pod in %s connected to the apiserver ClusterIP %s:443 — the default-deny egress policy "+
						"is not being enforced (policy missing or the CNI ignores it)", buildNamespace, apiVIP),
				}, nil
			}
			if phases[denyRegistry.Name] == corev1.PodSucceeded {
				return check.Result{
					Status: check.StatusFail,
					Message: fmt.Sprintf("an unlabeled pod in %s connected to the registry %s:443 even though its apiserver egress was blocked — "+
						"policy evaluation is partial (the registry ingress allowlist or the egress rule set is broken)", buildNamespace, registryVIP),
				}, nil
			}
			return check.Result{
				Status: check.StatusPass,
				Message: fmt.Sprintf("both unlabeled canaries were blocked (registry %s:443 and apiserver %s:443) while the build-labeled "+
					"control connected — the CNI enforces NetworkPolicy in both directions", registryVIP, apiVIP),
			}, nil
		},
	}
}

// serviceVIP reads a Service's ClusterIP — the canaries' connection targets.
func serviceVIP(ctx context.Context, c client.Client, namespace, name string) (string, error) {
	var svc corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &svc); err != nil {
		return "", fmt.Errorf("read Service %s/%s (a probe target): %w", namespace, name, err)
	}
	vip := svc.Spec.ClusterIP
	if vip == "" || vip == corev1.ClusterIPNone {
		return "", fmt.Errorf("service %s/%s has no ClusterIP to probe", namespace, name)
	}
	return vip, nil
}

// canaryDetail summarizes a canary's container state for error messages, so
// an ImagePullBackOff is not misreported as a connectivity problem.
func canaryDetail(ctx context.Context, c client.Client, name string) string {
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: buildNamespace, Name: name}, &pod); err != nil {
		return "state unreadable: " + err.Error()
	}
	detail := "pod phase " + string(pod.Status.Phase)
	if pod.Status.Reason != "" {
		detail += "/" + pod.Status.Reason
	}
	for _, cs := range pod.Status.ContainerStatuses {
		switch {
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			detail += ", container " + cs.State.Waiting.Reason
		case cs.State.Terminated != nil:
			detail += fmt.Sprintf(", container exited %d", cs.State.Terminated.ExitCode)
			if cs.State.Terminated.Reason != "" {
				detail += " (" + cs.State.Terminated.Reason + ")"
			}
		}
	}
	return detail
}

// canaryPod renders one connectivity canary: restricted-grade, no SA token,
// bounded lifetime, exit code = whether the TCP connect succeeded.
func canaryPod(name, appLabel, vip string) *corev1.Pod {
	no := false
	yes := true
	uid := int64(65534)
	deadline := int64(60)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: buildNamespace,
			Name:      name,
			Labels: map[string]string{
				"app.kubernetes.io/name":       appLabel,
				"app.kubernetes.io/managed-by": canaryManagedByValue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        &deadline,
			AutomountServiceAccountToken: &no,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &yes,
				RunAsUser:      &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   CanaryImage,
				Command: []string{"sh", "-c", fmt.Sprintf("nc -z -w 5 %s 443", vip)},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

// sweepCanaries removes leftovers a crashed earlier run may have stranded.
// Two CONCURRENT doctor runs could sweep each other's in-flight canaries;
// that degrades to a probe error, never a wrong verdict — accepted for a
// manually-invoked single-admin diagnostic.
func sweepCanaries(ctx context.Context, c client.Client) error {
	err := c.DeleteAllOf(ctx, &corev1.Pod{},
		client.InNamespace(buildNamespace),
		client.MatchingLabels{"app.kubernetes.io/managed-by": canaryManagedByValue})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("sweep leftover canary pods: %w", err)
	}
	return nil
}

// deleteCanary is best-effort cleanup on its own context: it must run even
// when the probe's ctx is already cancelled.
func deleteCanary(c client.Client, p *corev1.Pod) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.Delete(ctx, p)
}

// waitCanaryDone polls the canary until it reaches a terminal phase. The
// pod's activeDeadlineSeconds bounds its runtime (kubelet fails it even if
// the image never pulls), and the caller's wait budget bounds ours. A
// transient Get error keeps the poll going rather than aborting — the
// install/k3s waitReady convention — so an API hiccup mid-poll cannot fail a
// 3-minute probe; only the deadline does, surfacing the last observed state.
func waitCanaryDone(ctx context.Context, c client.Client, name string) (corev1.PodPhase, error) {
	var lastState string
	for {
		var pod corev1.Pod
		if err := c.Get(ctx, client.ObjectKey{Namespace: buildNamespace, Name: name}, &pod); err != nil {
			lastState = fmt.Sprintf("read failed: %v", err)
		} else {
			switch pod.Status.Phase {
			case corev1.PodSucceeded, corev1.PodFailed:
				return pod.Status.Phase, nil
			}
			lastState = fmt.Sprintf("phase %q", pod.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("canary did not finish in time (last state: %s): %w", lastState, ctx.Err())
		case <-time.After(netpolPollInterval):
		}
	}
}
