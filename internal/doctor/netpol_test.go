package doctor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/doctor"
)

// legState is how a canary "went". The probe classifies by the container's
// EXIT CODE, not the pod phase, so the fixtures must stamp both — a pod that
// merely reaches Failed carries no evidence either way.
type legState int

const (
	legPending  legState = iota // never reaches a terminal phase
	legConnects                 // exit 0: the connection went through
	legBlocks                   // exit 42: refused or timed out, i.e. policy held
	legWedged                   // terminal with no usable exit code (image never pulled)
)

func stampLeg(pod *corev1.Pod, s legState) {
	switch s {
	case legPending:
		pod.Status.Phase = corev1.PodPending
	case legConnects:
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = terminated(0, "Completed")
	case legBlocks:
		pod.Status.Phase = corev1.PodFailed
		pod.Status.ContainerStatuses = terminated(42, "Error")
	case legWedged:
		// The kubelet enforced activeDeadlineSeconds before the image ever
		// pulled: a Failed pod that proves nothing about connectivity.
		pod.Status.Phase = corev1.PodFailed
		pod.Status.Reason = "DeadlineExceeded"
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "probe",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		}}
	}
}

func terminated(code int32, reason string) []corev1.ContainerStatus {
	return []corev1.ContainerStatus{{
		Name: "probe",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason},
		},
	}}
}

func registryService(clusterIP string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-system", Name: "orkano-registry"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: clusterIP},
	}
}

func apiServerService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "kubernetes"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.43.0.1"},
	}
}

func buildsNetworkPolicy() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-builds", Name: "orkano-builds-default-deny"},
	}
}

// canaryRole recovers "control" / "deny-registry" / "deny-egress" from a canary
// name of the form orkano-doctor-netpol-<role>-<stamp>-a<attempt>.
func canaryRole(name string) string {
	switch {
	case strings.Contains(name, "-control-"):
		return "control"
	case strings.Contains(name, "-deny-registry-"):
		return "deny-registry"
	case strings.Contains(name, "-deny-egress-"):
		return "deny-egress"
	}
	return ""
}

// canaryOutcomes stamps a fixed outcome per role onto every batch.
func canaryOutcomes(control, denyRegistry, denyEgress legState) interceptor.Funcs {
	return canarySequence(func(role string, _ int) legState {
		switch role {
		case "control":
			return control
		case "deny-registry":
			return denyRegistry
		case "deny-egress":
			return denyEgress
		}
		return legPending
	})
}

// canarySequence plays the kubelet, deciding each canary's outcome from its
// role AND its batch number — the fake client runs no pods, so this is how a
// substrate that only starts enforcing on the second batch is expressed.
func canarySequence(outcome func(role string, attempt int) legState) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if pod, ok := obj.(*corev1.Pod); ok {
				if role := canaryRole(pod.Name); role != "" {
					stampLeg(pod, outcome(role, attemptOf(pod.Name)))
				}
			}
			return cl.Create(ctx, obj, opts...)
		},
	}
}

func attemptOf(name string) int {
	i := strings.LastIndex(name, "-a")
	if i < 0 {
		return 1
	}
	n := 0
	for _, r := range name[i+2:] {
		if r < '0' || r > '9' {
			return 1
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 1
	}
	return n
}

func netpolFake(t *testing.T, outcomes *interceptor.Funcs, extra ...client.Object) client.Client {
	t.Helper()
	// The default fixture carries a policy object: the probe's no-policy fast
	// path is a separate, explicitly-tested branch.
	objs := append([]client.Object{registryService("10.43.0.7"), apiServerService(), buildsNetworkPolicy()}, extra...)
	b := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...)
	if outcomes != nil {
		b = b.WithInterceptorFuncs(*outcomes)
	}
	return b.Build()
}

func netpolProbe(t *testing.T, c client.Client) (check.Result, error) {
	t.Helper()
	return probeCheck(t, doctor.Options{Client: c}, doctor.IDNetworkPolicyEnforced)
}

// shrinkNetpol runs the retry loop at test speed. Every netpol test that can
// reach a retry needs it, or the settle window is measured in real minutes.
func shrinkNetpol(t *testing.T) {
	t.Helper()
	restore := doctor.ShrinkNetpolTimingForTest(2*time.Second, time.Millisecond)
	t.Cleanup(restore)
}

func remainingCanaries(t *testing.T, c client.Client) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace("orkano-builds"),
		client.MatchingLabels{"app.kubernetes.io/managed-by": "orkano-doctor"}); err != nil {
		t.Fatalf("list canaries: %v", err)
	}
	return pods.Items
}

func TestNetworkPolicyEnforced(t *testing.T) {
	t.Run("both deny legs blocked while control connects passes", func(t *testing.T) {
		shrinkNetpol(t)
		o := canaryOutcomes(legConnects, legBlocks, legBlocks)
		c := netpolFake(t, &o)
		res, err := netpolProbe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "10.43.0.7") || !strings.Contains(res.Message, "10.43.0.1") {
			t.Errorf("message %q should name both probed VIPs", res.Message)
		}
		if left := remainingCanaries(t, c); len(left) != 0 {
			t.Errorf("canary pods not cleaned up: %d remain", len(left))
		}
	})

	// THE REGRESSION THIS LOOP EXISTS FOR. On a freshly booted single-node k3s
	// the policy controller has not programmed the pod firewall yet, so the
	// first batch leaks and a single-sample probe reported a definitive Fail on
	// a healthy install (~74% hardening score). A second fresh batch must be
	// able to settle it.
	t.Run("a leak that settles on a later batch passes", func(t *testing.T) {
		shrinkNetpol(t)
		o := canarySequence(func(role string, attempt int) legState {
			if role == "control" {
				return legConnects
			}
			if attempt == 1 {
				return legConnects // firewall not programmed yet
			}
			return legBlocks
		})
		c := netpolFake(t, &o)
		res, err := netpolProbe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass once enforcement settles", res.Status, res.Message)
		}
		if left := remainingCanaries(t, c); len(left) != 0 {
			t.Errorf("canary pods not cleaned up: %d remain", len(left))
		}
	})

	// The egress leg targets the apiserver, whose ingress nothing guards: an
	// unlabeled pod connecting there means the default-deny egress itself is
	// dead — even if the registry deny leg still blocks (its ingress
	// allowlist could mask a deleted egress policy).
	t.Run("a persistent apiserver leak fails as broken egress", func(t *testing.T) {
		shrinkNetpol(t)
		o := canaryOutcomes(legConnects, legBlocks, legConnects)
		c := netpolFake(t, &o)
		res, err := netpolProbe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "default-deny egress") {
			t.Errorf("message %q should attribute the failure to the egress policy", res.Message)
		}
		if !strings.Contains(res.Message, "independent canary batches") {
			t.Errorf("message %q should say the leak was reproduced", res.Message)
		}
	})

	t.Run("nothing enforced fails naming the egress leg", func(t *testing.T) {
		shrinkNetpol(t)
		o := canaryOutcomes(legConnects, legConnects, legConnects)
		c := netpolFake(t, &o)
		res, err := netpolProbe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "not being enforced") {
			t.Errorf("message %q should state that enforcement is missing", res.Message)
		}
		if left := remainingCanaries(t, c); len(left) != 0 {
			t.Errorf("canary pods not cleaned up: %d remain", len(left))
		}
	})

	t.Run("registry reachable despite blocked egress fails as partial evaluation", func(t *testing.T) {
		shrinkNetpol(t)
		o := canaryOutcomes(legConnects, legConnects, legBlocks)
		c := netpolFake(t, &o)
		res, err := netpolProbe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "partial") {
			t.Errorf("message %q should call out partial policy evaluation", res.Message)
		}
	})

	// With no policy object there is nothing to converge on, so the operator
	// gets the answer immediately instead of waiting out the settle window.
	t.Run("a leak with no policy present fails on the first batch", func(t *testing.T) {
		shrinkNetpol(t)
		o := canaryOutcomes(legConnects, legConnects, legConnects)
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(registryService("10.43.0.7"), apiServerService()).
			WithInterceptorFuncs(o).Build()
		res, err := netpolProbe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "no NetworkPolicy exists") {
			t.Errorf("message %q should name the missing policy", res.Message)
		}
		if strings.Contains(res.Message, "independent canary batches") {
			t.Errorf("a missing policy needs no confirming batch: %q", res.Message)
		}
	})

	// THE PRE-EXISTING FALSE-PASS HOLE. Classifying on pod phase alone read any
	// non-Succeeded deny canary as "blocked", so a deny pod whose image never
	// pulled counted as proof of enforcement. It must be unknown instead.
	t.Run("a wedged deny canary is indeterminate, never a pass", func(t *testing.T) {
		shrinkNetpol(t)
		o := canaryOutcomes(legConnects, legWedged, legWedged)
		c := netpolFake(t, &o)
		res, err := netpolProbe(t, c)
		if err == nil {
			t.Fatalf("expected a probe error, got status %q (%s)", res.Status, res.Message)
		}
		if !strings.Contains(err.Error(), "confident verdict") {
			t.Errorf("error %q should say the verdict is not confident", err)
		}
	})

	// If the ALLOWED path cannot connect, blocked deny canaries prove
	// nothing — the registry might just be down. Indeterminate, never a pass.
	t.Run("failed control leg is a probe error", func(t *testing.T) {
		shrinkNetpol(t)
		o := canaryOutcomes(legBlocks, legBlocks, legBlocks)
		c := netpolFake(t, &o)
		_, err := netpolProbe(t, c)
		if err == nil || !strings.Contains(err.Error(), "cannot attribute") {
			t.Fatalf("expected the attribution probe error, got %v", err)
		}
		if left := remainingCanaries(t, c); len(left) != 0 {
			t.Errorf("canary pods not cleaned up after error: %d remain", len(left))
		}
	})

	// A wedged image pull must be visible in the error: "check the registry"
	// alone would misdirect an air-gapped operator.
	t.Run("control failure surfaces the container state", func(t *testing.T) {
		shrinkNetpol(t)
		o := canaryOutcomes(legWedged, legBlocks, legBlocks)
		c := netpolFake(t, &o)
		_, err := netpolProbe(t, c)
		if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
			t.Fatalf("expected the container state in the probe error, got %v", err)
		}
	})

	t.Run("missing registry service is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		if _, err := netpolProbe(t, c); err == nil {
			t.Fatal("expected a probe error")
		}
	})

	t.Run("missing kubernetes service is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(registryService("10.43.0.7")).Build()
		_, err := netpolProbe(t, c)
		if err == nil || !strings.Contains(err.Error(), "default/kubernetes") {
			t.Fatalf("expected a probe error naming the apiserver Service, got %v", err)
		}
	})

	t.Run("refused pod create is a probe error", func(t *testing.T) {
		shrinkNetpol(t)
		o := interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					return errors.New("pods is forbidden")
				}
				return cl.Create(ctx, obj, opts...)
			},
		}
		c := netpolFake(t, &o)
		if _, err := netpolProbe(t, c); err == nil {
			t.Fatal("expected a probe error")
		}
	})

	t.Run("leftover canaries from a crashed run are swept", func(t *testing.T) {
		shrinkNetpol(t)
		leftover := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "orkano-builds",
			Name:      "orkano-doctor-netpol-deny-registry-11111-a1",
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "orkano-doctor"},
		}}
		o := canaryOutcomes(legConnects, legBlocks, legBlocks)
		c := netpolFake(t, &o, leftover)
		if _, err := netpolProbe(t, c); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if left := remainingCanaries(t, c); len(left) != 0 {
			t.Errorf("leftover canary not swept: %d remain", len(left))
		}
	})

	// A canary that never reaches a terminal phase (image pull wedged, node
	// gone) must bound the probe: an error after the wait budget, never a
	// hang and never a verdict.
	t.Run("canary that never finishes times out as a probe error", func(t *testing.T) {
		restore := doctor.ShrinkNetpolTimingForTest(50*time.Millisecond, 5*time.Millisecond)
		defer restore()
		c := netpolFake(t, nil) // no outcome stamping: pods stay Pending
		_, err := netpolProbe(t, c)
		if err == nil || !strings.Contains(err.Error(), "did not finish in time") {
			t.Fatalf("expected the timeout probe error, got %v", err)
		}
		if left := remainingCanaries(t, c); len(left) != 0 {
			t.Errorf("canary pods not cleaned up after timeout: %d remain", len(left))
		}
	})

	// A transient API hiccup mid-poll must not abort a minutes-long probe —
	// the install/k3s waitReady convention; only the deadline fails the wait.
	t.Run("transient get errors during the wait are tolerated", func(t *testing.T) {
		shrinkNetpol(t)
		flaky := 4
		o := canaryOutcomes(legConnects, legBlocks, legBlocks)
		o.Get = func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Pod); ok && flaky > 0 {
				flaky--
				return errors.New("transient apiserver hiccup")
			}
			return cl.Get(ctx, key, obj, opts...)
		}
		c := netpolFake(t, &o)
		res, err := netpolProbe(t, c)
		if err != nil {
			t.Fatalf("probe should ride out transient errors, got %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
	})

	t.Run("headless registry service is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(registryService("None")).Build()
		if _, err := netpolProbe(t, c); err == nil {
			t.Fatal("expected a probe error for a Service without a usable ClusterIP")
		}
	})
}

// TestNetpolTimingInvariants pins the two arithmetic relationships the retry
// loop depends on. Break either and every canary reads indeterminate, or a
// batch is started that cannot finish inside the ceiling.
func TestNetpolTimingInvariants(t *testing.T) {
	live := doctor.LiveNetpolTiming()
	const ncTimeout = 5 * time.Second
	if live.CanaryDeadline <= live.CanarySettle+ncTimeout {
		t.Errorf("CanaryDeadline %s must exceed CanarySettle %s plus the nc timeout %s, or every canary is killed mid-probe",
			live.CanaryDeadline, live.CanarySettle, ncTimeout)
	}
	if live.BatchReserve < live.CanaryDeadline+2*live.PollInterval {
		t.Errorf("BatchReserve %s must cover CanaryDeadline %s plus two polls, or a started batch cannot finish inside the ceiling",
			live.BatchReserve, live.CanaryDeadline)
	}
	if live.MinLeakBatches > live.MaxBatches {
		t.Errorf("MinLeakBatches %d can never be reached within MaxBatches %d", live.MinLeakBatches, live.MaxBatches)
	}
}

// TestCanaryCommand pins the canary's contract: it settles before its first
// packet, reads its target from the environment rather than an interpolated
// shell word, and reports through the explicit exit-code convention.
func TestCanaryCommand(t *testing.T) {
	cmd := doctor.CanaryCommandForTest(5 * time.Second)
	for _, want := range []string{"sleep 5", `"$ORKANO_PROBE_TARGET"`, "0) exit 0", "1) exit 42", "*) exit 43"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("canary command %q missing %q", cmd, want)
		}
	}
}
