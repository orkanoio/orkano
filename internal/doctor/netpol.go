package doctor

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
)

// IDNetworkPolicyEnforced is PERMANENT — it appears in --json output and CI
// configs.
const IDNetworkPolicyEnforced = "net.networkpolicy-enforced"

// CanaryImage runs the connectivity canaries. Digest-pinned multi-arch INDEX
// (amd64+arm64), a Go constant like buildjob.DefaultImage — Renovate does not
// bump it; re-resolve the index digest deliberately.
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

	// probeTargetEnv carries the VIP as data rather than interpolating it into
	// the canary's shell command.
	probeTargetEnv = "ORKANO_PROBE_TARGET"
)

// Canary exit codes, byte-for-byte the convention internal/preflight/cluster
// uses: an explicit code per outcome, so "the script ran to completion and the
// connection was refused" is distinguishable from "the script never ran".
const (
	canaryExitConnected int32 = 0
	canaryExitBlocked   int32 = 42
	canaryExitInvalid   int32 = 43
)

// NetpolTiming is the netpol probe's schedule. It is a struct so a test can
// shrink one knob without restating the rest.
//
// Tuning invariants, both pinned by TestNetpolTimingInvariants:
//   - CanaryDeadline > CanarySettle + the nc -w timeout, or every canary is
//     killed mid-sleep and the whole probe reads indeterminate.
//   - BatchReserve >= CanaryDeadline + 2*PollInterval, so a batch started with
//     the reserve available always finishes inside the overall ceiling.
type NetpolTiming struct {
	// WaitBudget is the hard ceiling on the entire probe.
	WaitBudget time.Duration
	// PollInterval is how often a canary's phase is re-read.
	PollInterval time.Duration
	// CanarySettle is how long a canary sleeps before its first packet.
	CanarySettle time.Duration
	// CanaryDeadline bounds one canary's lifetime (activeDeadlineSeconds).
	CanaryDeadline time.Duration
	// SettleBudget is the retry window, measured from the end of the first
	// batch that did not pass.
	SettleBudget time.Duration
	// BatchReserve is the time a new batch needs; a batch is never started
	// without it remaining under WaitBudget.
	BatchReserve time.Duration
	// MinLeakBatches is how many independent fresh batches must leak before a
	// definitive Fail is rendered.
	MinLeakBatches int
	// MaxBatches caps how many batches one probe may run. SettleBudget is the
	// binding constraint in practice; this is the safety net that stops a
	// pathologically fast failure loop from creating pods without bound.
	MaxBatches int
}

var netpolTiming = NetpolTiming{
	WaitBudget:     3 * time.Minute,
	PollInterval:   2 * time.Second,
	CanarySettle:   5 * time.Second,
	CanaryDeadline: 60 * time.Second,
	SettleBudget:   90 * time.Second,
	BatchReserve:   70 * time.Second,
	MinLeakBatches: 2,
	MaxBatches:     6,
}

// policyControllerNote is appended to terminal messages. On k3s the policy
// controller is kube-router running INSIDE the k3s server process — there is no
// DaemonSet and no pod to inspect — and `--disable-network-policy` turns it off
// silently, so an operator hunting for a kube-system pod finds nothing.
// Explanation only: it is never read back into a verdict.
const policyControllerNote = " (on k3s the policy controller runs inside the server process, not as a pod: check `journalctl -u k3s` and whether the node was started with --disable-network-policy)"

// legOutcome is what one canary observed. The three-way split is load-bearing
// under a retry loop: classifying by pod phase alone reads a wedged canary
// (ImagePullBackOff into activeDeadlineSeconds) as "blocked", which would end
// the loop on a false Pass.
type legOutcome int

const (
	legIndeterminate legOutcome = iota
	legConnected
	legBlocked
)

func (o legOutcome) String() string {
	switch o {
	case legConnected:
		return "connected"
	case legBlocked:
		return "blocked"
	default:
		return "indeterminate"
	}
}

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
// A single sample is not enough. On a freshly booted single-node k3s the
// policy controller has not finished its first sync, so a canary scheduled at
// t=0 leaks through a window no real build occupies, and the probe used to
// report a definitive Fail on a healthy install. Two things fix that without
// weakening the verdict: each canary sleeps CanarySettle before its first
// packet (the same shape a BuildKit Job has — its first syscall is many
// seconds into its life), and a leak is re-tested with a FRESH batch until
// either the legs block (Pass) or the settle window closes. Fresh pods are
// mandatory: NetworkPolicy effects on existing connections are
// implementation-defined, which is why internal/preflight/cluster does the
// same.
//
// The verdict is never softened. A leak confirmed by MinLeakBatches
// independent batches is a Fail; anything still ambiguous when the window
// closes is a probe ERROR, because "unknown never counts as hardened".
// Retrying only ever delays a negative — it can never turn one into a Pass.
//
// Critical severity — if this fails, the build sandbox has no network
// boundary. The probe creates short-lived pods (restricted-grade, no SA token)
// and deletes each batch afterwards; it is safe to re-run and sweeps leftovers
// from crashed runs by label first.
func networkPolicyEnforcedCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDNetworkPolicyEnforced,
		Severity: check.SeverityCritical,
		Summary:  "the CNI enforces NetworkPolicy (capability-probed, INV-02 substrate)",
		Remediation: "run `kubectl get networkpolicy -n orkano-builds`: if the policies are missing, re-apply config/netpol/; " +
			"if they exist, the CNI is not enforcing them: on the stock k3s install kube-router's netpol controller must be running " +
			"(re-run `orkano init`), on a custom CNI install one that enforces NetworkPolicy",
		Probe: func(ctx context.Context) (check.Result, error) {
			return probeNetworkPolicy(ctx, opt)
		},
	}
}

func probeNetworkPolicy(ctx context.Context, opt Options) (check.Result, error) {
	registryVIP, err := serviceVIP(ctx, opt.Client, systemNamespace, registryServiceName)
	if err != nil {
		return check.Result{}, err
	}
	apiVIP, err := serviceVIP(ctx, opt.Client, "default", "kubernetes")
	if err != nil {
		return check.Result{}, err
	}

	// A fail-only fast path. With no policy objects there is nothing for the
	// CNI to converge on, so the first leak is already definitive and the
	// operator gets an answer in seconds instead of minutes. It can only make
	// a negative faster — never turn a leak into a Pass — which is why reading
	// configuration here does not undercut the capability probe.
	policiesPresent, policiesKnown := networkPoliciesPresent(ctx, opt.Client)

	if err := sweepCanaries(ctx, opt.Client); err != nil {
		return check.Result{}, err
	}

	// Loop control uses time.Now() and context deadlines, NEVER opt.now():
	// tests inject a FROZEN clock, under which a settle deadline derived from
	// opt.now() is never reached and the probe hangs.
	total, cancel := context.WithTimeout(ctx, netpolTiming.WaitBudget)
	defer cancel()
	ceiling := time.Now().Add(netpolTiming.WaitBudget)

	var (
		settleDeadline time.Time
		leaks          int
		last           *batchResult
	)
	for attempt := 1; ; attempt++ {
		b, err := runCanaryBatch(total, opt, attempt, registryVIP, apiVIP)
		if err != nil {
			return check.Result{}, err
		}
		last = b

		// The control leg is the probe's own health gate: if the ALLOWED path
		// cannot connect, blocked deny canaries prove nothing (the registry may
		// simply be down), so the result is indeterminate.
		if b.control != legConnected {
			return check.Result{}, fmt.Errorf(
				"the allowed control path (build-labeled pod -> registry %s:443) did not connect (%s): cannot attribute the deny results to policy; "+
					"check the registry's health, and whether the canary image %s is pullable on the nodes (air-gapped or rate-limited installs need it preloaded)",
				registryVIP, b.controlDetail, CanaryImage)
		}

		if b.denyEgress == legBlocked && b.denyRegistry == legBlocked {
			return check.Result{
				Status: check.StatusPass,
				Message: fmt.Sprintf("both unlabeled canaries were blocked (registry %s:443 and apiserver %s:443) while the build-labeled "+
					"control connected: the CNI enforces NetworkPolicy in both directions", registryVIP, apiVIP),
			}, nil
		}

		if b.leaked() {
			leaks++
			// Nothing to wait for: no policy exists, so no amount of settling
			// will start enforcement.
			if policiesKnown && !policiesPresent {
				return netpolFailResult(b, registryVIP, apiVIP,
					"; no NetworkPolicy exists in "+buildNamespace+", so nothing is being enforced; re-apply config/netpol/"), nil
			}
		}

		if settleDeadline.IsZero() {
			settleDeadline = time.Now().Add(netpolTiming.SettleBudget)
		}
		now := time.Now()
		roomToRetry := attempt < netpolTiming.MaxBatches &&
			now.Before(settleDeadline) &&
			now.Add(netpolTiming.BatchReserve).Before(ceiling) &&
			total.Err() == nil
		if !roomToRetry {
			break
		}
	}

	// The window closed. A leak reproduced across independent fresh batches is
	// definitive; anything else is unknown, and unknown never counts as
	// hardened.
	if leaks >= netpolTiming.MinLeakBatches && last.leaked() {
		return netpolFailResult(last, registryVIP, apiVIP,
			fmt.Sprintf(", reproduced across %d independent canary batches over %s", leaks, netpolTiming.SettleBudget)), nil
	}
	return check.Result{}, fmt.Errorf(
		"could not reach a confident verdict within %s: last batch saw the registry leg %s and the apiserver leg %s (control connected); "+
			"if the cluster booted moments ago the policy controller may still be programming the pod firewall; re-run `orkano doctor`%s",
		netpolTiming.WaitBudget, last.denyRegistry, last.denyEgress, policyControllerNote)
}

// netpolFailResult renders the definitive negative, naming the leg that leaked.
// The apiserver leg is reported first: it is the one that isolates the
// default-deny egress, so it names the real defect when both leak.
func netpolFailResult(b *batchResult, registryVIP, apiVIP, suffix string) check.Result {
	var msg string
	switch b.denyEgress {
	case legConnected:
		msg = fmt.Sprintf("an unlabeled pod in %s connected to the apiserver ClusterIP %s:443: the default-deny egress policy "+
			"is not being enforced (policy missing or the CNI ignores it)", buildNamespace, apiVIP)
	default:
		msg = fmt.Sprintf("an unlabeled pod in %s connected to the registry %s:443 even though its apiserver egress was blocked: "+
			"policy evaluation is partial (the registry ingress allowlist or the egress rule set is broken)", buildNamespace, registryVIP)
	}
	return check.Result{Status: check.StatusFail, Message: msg + suffix + policyControllerNote}
}

// batchResult is one round of three fresh canaries.
type batchResult struct {
	control       legOutcome
	denyRegistry  legOutcome
	denyEgress    legOutcome
	controlDetail string
}

// leaked reports whether a deny leg connected — the only observation that can
// become a Fail. An indeterminate leg is neither a leak nor a pass.
func (b *batchResult) leaked() bool {
	return b.denyRegistry == legConnected || b.denyEgress == legConnected
}

// runCanaryBatch creates, waits for, classifies and deletes one fresh triple.
// Cleanup runs after classification: the terminal pods ARE the evidence.
func runCanaryBatch(ctx context.Context, opt Options, attempt int, registryVIP, apiVIP string) (*batchResult, error) {
	// The name carries both the run stamp and the batch, so a lagging delete
	// from the previous batch can never collide with this one.
	stamp := opt.now().Unix()
	name := func(role string) string {
		return fmt.Sprintf("orkano-doctor-netpol-%s-%d-a%d", role, stamp, attempt)
	}
	control := canaryPod(name("control"), buildPodLabel, registryVIP)
	denyRegistry := canaryPod(name("deny-registry"), canaryLabelValue, registryVIP)
	denyEgress := canaryPod(name("deny-egress"), canaryLabelValue, apiVIP)
	pods := []*corev1.Pod{control, denyRegistry, denyEgress}

	cleanup := func() {
		for _, p := range pods {
			deleteCanary(opt.Client, p)
		}
	}
	for _, p := range pods {
		if err := opt.Client.Create(ctx, p); err != nil {
			cleanup()
			return nil, fmt.Errorf("create canary pod %s: %w", p.Name, err)
		}
	}
	defer cleanup()

	out := &batchResult{}
	for _, p := range pods {
		outcome, detail, err := waitCanaryOutcome(ctx, opt.Client, p.Name)
		if err != nil {
			return nil, fmt.Errorf("wait for canary %s (%s): %w", p.Name, detail, err)
		}
		switch p.Name {
		case control.Name:
			out.control, out.controlDetail = outcome, detail
		case denyRegistry.Name:
			out.denyRegistry = outcome
		case denyEgress.Name:
			out.denyEgress = outcome
		}
	}
	return out, nil
}

// networkPoliciesPresent reports whether orkano-builds holds any policy, and
// whether the answer is known at all. A list failure falls through to probing:
// the gate is an optimisation, and skipping it can never produce a wrong
// verdict.
func networkPoliciesPresent(ctx context.Context, c client.Client) (present, known bool) {
	var policies networkingv1.NetworkPolicyList
	if err := c.List(ctx, &policies, client.InNamespace(buildNamespace)); err != nil {
		return false, false
	}
	return len(policies.Items) > 0, true
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

// podDetail summarizes a pod's container state for messages, so an
// ImagePullBackOff is not misreported as a connectivity problem.
func podDetail(pod *corev1.Pod) string {
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

// canaryCommand sleeps before its first packet so the canary resembles the
// build Job it stands in for, then maps the connect result onto the explicit
// exit-code convention. The target arrives via the environment, so it is data
// rather than something interpolated into a shell word.
func canaryCommand(settle time.Duration) string {
	seconds := int(settle.Round(time.Second) / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	return fmt.Sprintf(
		`sleep %d; nc -z -w 5 "$%s" 443; case $? in 0) exit %d;; 1) exit %d;; *) exit %d;; esac`,
		seconds, probeTargetEnv, canaryExitConnected, canaryExitBlocked, canaryExitInvalid)
}

// canaryPod renders one connectivity canary: restricted-grade, no SA token,
// bounded lifetime, exit code = whether the TCP connect succeeded.
func canaryPod(name, appLabel, vip string) *corev1.Pod {
	no := false
	yes := true
	uid := int64(65534)
	deadline := int64(netpolTiming.CanaryDeadline.Round(time.Second) / time.Second)
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
				Command: []string{"sh", "-c", canaryCommand(netpolTiming.CanarySettle)},
				Env:     []corev1.EnvVar{{Name: probeTargetEnv, Value: vip}},
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

// waitCanaryOutcome polls the canary until it reaches a terminal phase, then
// classifies it by the container's EXIT CODE rather than the pod phase.
// Classifying on phase alone would read any non-Succeeded pod as "blocked", so
// a wedged image pull would count as evidence of enforcement.
//
// The pod's activeDeadlineSeconds bounds its runtime (kubelet fails it even if
// the image never pulls), and the caller's budget bounds ours. A transient Get
// error keeps the poll going rather than aborting — the install/k3s waitReady
// convention — so an API hiccup mid-poll cannot fail a minutes-long probe;
// only the deadline does, surfacing the last observed state.
func waitCanaryOutcome(ctx context.Context, c client.Client, name string) (legOutcome, string, error) {
	var lastState string
	for {
		var pod corev1.Pod
		if err := c.Get(ctx, client.ObjectKey{Namespace: buildNamespace, Name: name}, &pod); err != nil {
			lastState = fmt.Sprintf("read failed: %v", err)
		} else {
			switch pod.Status.Phase {
			case corev1.PodSucceeded, corev1.PodFailed:
				return classifyCanary(&pod), podDetail(&pod), nil
			}
			lastState = fmt.Sprintf("phase %q", pod.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return legIndeterminate, lastState, fmt.Errorf("canary did not finish in time (last state: %s): %w", lastState, ctx.Err())
		case <-time.After(netpolTiming.PollInterval):
		}
	}
}

// classifyCanary maps a terminal pod onto its leg outcome. Only the two
// codes the canary script emits deliberately are evidence; everything else —
// a kubelet-enforced deadline, an OOM kill, an image that never pulled, a
// shell that could not run nc — is unknown.
func classifyCanary(pod *corev1.Pod) legOutcome {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "probe" || cs.State.Terminated == nil {
			continue
		}
		switch cs.State.Terminated.ExitCode {
		case canaryExitConnected:
			return legConnected
		case canaryExitBlocked:
			return legBlocked
		default:
			return legIndeterminate
		}
	}
	return legIndeterminate
}
