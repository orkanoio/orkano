package cluster_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/preflight/cluster"
)

const probeRoleLabel = "orkano.io/preflight-role"

type liveRecorder struct {
	namespaces []string
	pods       []*corev1.Pod
	policies   []*networkingv1.NetworkPolicy
}

func readyLinuxNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"kubernetes.io/os":       "linux",
				"kubernetes.io/hostname": name,
			},
		},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
		}}},
	}
}

func terminalPod(pod *corev1.Pod, exitCode int32) {
	if exitCode == 0 {
		pod.Status.Phase = corev1.PodSucceeded
	} else {
		pod.Status.Phase = corev1.PodFailed
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "probe",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: exitCode,
		}},
	}}
}

func healthyLivePod(pod *corev1.Pod) error {
	switch pod.Labels[probeRoleLabel] {
	case "netpol-server":
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = "10.244.0.9"
	case "netpol-control":
		terminalPod(pod, 0)
	case "netpol-deny":
		if strings.HasPrefix(pod.GenerateName, "baseline-deny-") {
			terminalPod(pod, 0)
		} else {
			terminalPod(pod, 42)
		}
	case "apparmor", "seccomp":
		terminalPod(pod, 0)
	case "psa":
		return podSecurityForbidden(pod)
	}
	return nil
}

func podSecurityForbidden(pod *corev1.Pod) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod.Name, errors.New(`violates PodSecurity "restricted:latest"`))
}

func liveClient(t *testing.T, objs []client.Object, onPod func(*corev1.Pod) error) (client.Client, *liveRecorder) {
	t.Helper()
	recorder := &liveRecorder{}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			switch typed := obj.(type) {
			case *corev1.Pod:
				if onPod != nil {
					if err := onPod(typed); err != nil {
						return err
					}
				}
				recorder.pods = append(recorder.pods, typed.DeepCopy())
				return cl.Create(ctx, typed, opts...)
			case *corev1.Namespace:
				err := cl.Create(ctx, typed, opts...)
				if err == nil {
					recorder.namespaces = append(recorder.namespaces, typed.Name)
				}
				return err
			case *networkingv1.NetworkPolicy:
				recorder.policies = append(recorder.policies, typed.DeepCopy())
				return cl.Create(ctx, typed, opts...)
			default:
				return cl.Create(ctx, obj, opts...)
			}
		},
	}).Build()
	return c, recorder
}

func probeLive(t *testing.T, c client.Client, id string) (check.Result, error) {
	t.Helper()
	return probeCheck(t, cluster.Options{Client: c}, id)
}

func assertScratchNamespacesDeleted(t *testing.T, c client.Client, recorder *liveRecorder) {
	t.Helper()
	if len(recorder.namespaces) == 0 {
		t.Fatal("expected the live probe to create a scratch namespace")
	}
	for _, name := range recorder.namespaces {
		var ns corev1.Namespace
		err := c.Get(context.Background(), client.ObjectKey{Name: name}, &ns)
		if !apierrors.IsNotFound(err) {
			t.Errorf("scratch namespace %s still exists or could not be read: %v", name, err)
		}
	}
}

func canaryNode(pod *corev1.Pod) string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil || pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchFields) != 1 || len(terms[0].MatchFields[0].Values) != 1 {
		return ""
	}
	field := terms[0].MatchFields[0]
	if field.Key != "metadata.name" || field.Operator != corev1.NodeSelectorOpIn {
		return ""
	}
	return field.Values[0]
}

func TestNetworkPolicyEnforced(t *testing.T) {
	t.Run("allowed control connects and denied canary is blocked", func(t *testing.T) {
		c, recorder := liveClient(t, []client.Object{readyLinuxNode("worker-a"), readyLinuxNode("worker-b")}, healthyLivePod)
		res, err := probeLive(t, c, cluster.IDNetworkPolicyEnforced)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		assertScratchNamespacesDeleted(t, c, recorder)

		roles := map[string]bool{}
		targets := map[string]map[string]bool{}
		baselineConnected := false
		confirmationConnected := false
		for _, pod := range recorder.pods {
			role := pod.Labels[probeRoleLabel]
			roles[role] = true
			if role != "netpol-server" {
				stage := role
				if role == "netpol-deny" && strings.HasPrefix(pod.GenerateName, "baseline-deny-") {
					stage = "baseline-deny"
				}
				if targets[stage] == nil {
					targets[stage] = map[string]bool{}
				}
				targets[stage][canaryNode(pod)] = true
			}
			if role == "netpol-deny" && strings.HasPrefix(pod.GenerateName, "baseline-deny-") && pod.Status.Phase == corev1.PodSucceeded {
				baselineConnected = true
			}
			if role == "netpol-control" && strings.HasPrefix(pod.GenerateName, "confirm-control-") && pod.Status.Phase == corev1.PodSucceeded {
				confirmationConnected = true
			}
			if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
				t.Errorf("%s canary must not mount a ServiceAccount token", pod.Labels[probeRoleLabel])
			}
			if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Errorf("%s canary must be restricted-grade", pod.Labels[probeRoleLabel])
			}
		}
		for _, role := range []string{"netpol-server", "netpol-control", "netpol-deny"} {
			if !roles[role] {
				t.Errorf("missing %s canary", role)
			}
		}
		if !baselineConnected {
			t.Error("probe must prove the denied source can connect before creating its NetworkPolicies")
		}
		if !confirmationConnected {
			t.Error("probe must confirm the allowed path after the denied canaries run")
		}
		for _, role := range []string{"baseline-deny", "netpol-control", "netpol-deny"} {
			for _, node := range []string{"worker-a", "worker-b"} {
				if !targets[role][node] {
					t.Errorf("missing %s canary pinned to %s; targets were %v", role, node, targets[role])
				}
			}
		}
		if len(recorder.policies) != 2 {
			t.Fatalf("created %d NetworkPolicies, want default deny plus control allow", len(recorder.policies))
		}
		var defaultDeny, allowControl *networkingv1.NetworkPolicy
		for _, policy := range recorder.policies {
			switch policy.Name {
			case "default-deny-egress":
				defaultDeny = policy
			case "allow-control-egress":
				allowControl = policy
			}
		}
		if defaultDeny == nil || len(defaultDeny.Spec.PodSelector.MatchLabels) != 0 || len(defaultDeny.Spec.Egress) != 0 || len(defaultDeny.Spec.PolicyTypes) != 1 || defaultDeny.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
			t.Errorf("default-deny-egress shape = %+v, want all-pod zero-rule Egress policy", defaultDeny)
		}
		if allowControl == nil || allowControl.Spec.PodSelector.MatchLabels[probeRoleLabel] != "netpol-control" || len(allowControl.Spec.Egress) != 1 || len(allowControl.Spec.Egress[0].To) != 1 || allowControl.Spec.Egress[0].To[0].PodSelector == nil || allowControl.Spec.Egress[0].To[0].PodSelector.MatchLabels[probeRoleLabel] != "netpol-server" {
			t.Errorf("allow-control-egress shape = %+v, want control-only egress to the server", allowControl)
		}
	})

	t.Run("denied canary that always connects fails after propagation budget", func(t *testing.T) {
		restore := cluster.SetLiveProbeTimingForTest(35*time.Millisecond, time.Millisecond)
		defer restore()
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a")}, func(pod *corev1.Pod) error {
			switch pod.Labels[probeRoleLabel] {
			case "netpol-server":
				pod.Status.Phase = corev1.PodRunning
				pod.Status.PodIP = "10.244.0.9"
			case "netpol-control", "netpol-deny":
				terminalPod(pod, 0)
			}
			return nil
		})
		res, err := probeLive(t, c, cluster.IDNetworkPolicyEnforced)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail || !strings.Contains(res.Message, "not enforcing") {
			t.Fatalf("status/message = %q/%q, want failed enforcement verdict", res.Status, res.Message)
		}
	})

	t.Run("a stale healthy control does not make a later broken control a definitive failure", func(t *testing.T) {
		restore := cluster.SetLiveProbeTimingForTest(35*time.Millisecond, time.Millisecond)
		defer restore()
		controlCalls := 0
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a")}, func(pod *corev1.Pod) error {
			switch pod.Labels[probeRoleLabel] {
			case "netpol-server":
				pod.Status.Phase = corev1.PodRunning
				pod.Status.PodIP = "10.244.0.9"
			case "netpol-control":
				controlCalls++
				if controlCalls == 1 {
					terminalPod(pod, 0)
				} else {
					terminalPod(pod, 42)
				}
			case "netpol-deny":
				terminalPod(pod, 0)
			}
			return nil
		})
		if _, err := probeLive(t, c, cluster.IDNetworkPolicyEnforced); err == nil || !strings.Contains(err.Error(), "latest NetworkPolicy canary batch") {
			t.Fatalf("expected an indeterminate latest-control error, got %v", err)
		}
	})

	t.Run("unusable control is an error, not a false policy verdict", func(t *testing.T) {
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a")}, func(pod *corev1.Pod) error {
			switch pod.Labels[probeRoleLabel] {
			case "netpol-server":
				pod.Status.Phase = corev1.PodRunning
				pod.Status.PodIP = "10.244.0.9"
			case "netpol-deny":
				if strings.HasPrefix(pod.GenerateName, "baseline-deny-") {
					terminalPod(pod, 0)
				} else {
					terminalPod(pod, 42)
				}
			case "netpol-control":
				terminalPod(pod, 43)
			}
			return nil
		})
		if _, err := probeLive(t, c, cluster.IDNetworkPolicyEnforced); err == nil || !strings.Contains(err.Error(), "allowed NetworkPolicy canary") {
			t.Fatalf("expected an indeterminate control error, got %v", err)
		}
	})

	t.Run("pending canary is bounded as an error", func(t *testing.T) {
		restore := cluster.SetLiveProbeTimingForTest(25*time.Millisecond, time.Millisecond)
		defer restore()
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a")}, func(pod *corev1.Pod) error {
			if pod.Labels[probeRoleLabel] == "netpol-server" {
				pod.Status.Phase = corev1.PodRunning
				pod.Status.PodIP = "10.244.0.9"
			}
			if pod.Labels[probeRoleLabel] == "netpol-deny" && strings.HasPrefix(pod.GenerateName, "baseline-deny-") {
				terminalPod(pod, 0)
			}
			return nil
		})
		if _, err := probeLive(t, c, cluster.IDNetworkPolicyEnforced); err == nil || !strings.Contains(err.Error(), "did not reach a terminal phase") {
			t.Fatalf("expected bounded pending-canary error, got %v", err)
		}
	})

	t.Run("pre-policy denied source must connect before enforcement is attributed", func(t *testing.T) {
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a")}, func(pod *corev1.Pod) error {
			switch pod.Labels[probeRoleLabel] {
			case "netpol-server":
				pod.Status.Phase = corev1.PodRunning
				pod.Status.PodIP = "10.244.0.9"
			case "netpol-deny":
				terminalPod(pod, 42)
			}
			return nil
		})
		if _, err := probeLive(t, c, cluster.IDNetworkPolicyEnforced); err == nil || !strings.Contains(err.Error(), "pre-policy deny-label") {
			t.Fatalf("expected pre-policy attribution error, got %v", err)
		}
	})

	t.Run("a failure on any eligible source node cannot hide behind another node", func(t *testing.T) {
		restore := cluster.SetLiveProbeTimingForTest(35*time.Millisecond, time.Millisecond)
		defer restore()
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a"), readyLinuxNode("worker-b")}, func(pod *corev1.Pod) error {
			switch pod.Labels[probeRoleLabel] {
			case "netpol-server":
				pod.Status.Phase = corev1.PodRunning
				pod.Status.PodIP = "10.244.0.9"
			case "netpol-control":
				terminalPod(pod, 0)
			case "netpol-deny":
				if strings.HasPrefix(pod.GenerateName, "baseline-deny-") || canaryNode(pod) == "worker-a" {
					terminalPod(pod, 0)
				} else {
					terminalPod(pod, 42)
				}
			}
			return nil
		})
		res, err := probeLive(t, c, cluster.IDNetworkPolicyEnforced)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail || !strings.Contains(res.Message, "worker-a") {
			t.Fatalf("status/message = %q/%q, want worker-a enforcement failure", res.Status, res.Message)
		}
	})

	t.Run("a server loss after the denied canaries is indeterminate", func(t *testing.T) {
		restore := cluster.SetLiveProbeTimingForTest(35*time.Millisecond, time.Millisecond)
		defer restore()
		controlCalls := 0
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a")}, func(pod *corev1.Pod) error {
			switch pod.Labels[probeRoleLabel] {
			case "netpol-server":
				pod.Status.Phase = corev1.PodRunning
				pod.Status.PodIP = "10.244.0.9"
			case "netpol-control":
				controlCalls++
				if controlCalls == 1 {
					terminalPod(pod, 0)
				} else {
					terminalPod(pod, 42)
				}
			case "netpol-deny":
				if strings.HasPrefix(pod.GenerateName, "baseline-deny-") {
					terminalPod(pod, 0)
				} else {
					terminalPod(pod, 42)
				}
			}
			return nil
		})
		if _, err := probeLive(t, c, cluster.IDNetworkPolicyEnforced); err == nil || !strings.Contains(err.Error(), "latest NetworkPolicy canary batch") {
			t.Fatalf("expected an indeterminate post-deny control error, got %v", err)
		}
	})
}

func TestPodSecurityAdmissionEnforced(t *testing.T) {
	t.Run("PodSecurity rejection passes and privileged canary is gated", func(t *testing.T) {
		var privileged *corev1.Pod
		c, recorder := liveClient(t, nil, func(pod *corev1.Pod) error {
			privileged = pod.DeepCopy()
			return podSecurityForbidden(pod)
		})
		res, err := probeLive(t, c, cluster.IDPodSecurityAdmissionEnforced)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		if privileged == nil || len(privileged.Spec.SchedulingGates) != 1 || privileged.Spec.SchedulingGates[0].Name != "orkano.io/preflight-psa" {
			t.Fatalf("privileged detector must be scheduling-gated, got %+v", privileged)
		}
		if privileged.Spec.Containers[0].SecurityContext.Privileged == nil || !*privileged.Spec.Containers[0].SecurityContext.Privileged {
			t.Fatal("PSA detector must be privileged")
		}
		assertScratchNamespacesDeleted(t, c, recorder)
	})

	t.Run("accepted privileged canary fails", func(t *testing.T) {
		c, _ := liveClient(t, nil, nil)
		res, err := probeLive(t, c, cluster.IDPodSecurityAdmissionEnforced)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail || !strings.Contains(res.Message, "privileged pod was admitted") {
			t.Fatalf("status/message = %q/%q, want PSA failure", res.Status, res.Message)
		}
	})

	t.Run("unrelated forbidden response is indeterminate", func(t *testing.T) {
		c, _ := liveClient(t, nil, func(pod *corev1.Pod) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod.Name, errors.New("RBAC denied"))
		})
		if _, err := probeLive(t, c, cluster.IDPodSecurityAdmissionEnforced); err == nil || !strings.Contains(err.Error(), "create privileged PSA canary") {
			t.Fatalf("expected an indeterminate forbidden response, got %v", err)
		}
	})

	t.Run("namespace still present after the cleanup deadline is indeterminate", func(t *testing.T) {
		restoreTiming := cluster.SetLiveProbeTimingForTest(time.Second, time.Millisecond)
		defer restoreTiming()
		restoreCleanup := cluster.SetScratchCleanupTimeoutForTest(20 * time.Millisecond)
		defer restoreCleanup()
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if pod, ok := obj.(*corev1.Pod); ok {
					return podSecurityForbidden(pod)
				}
				return cl.Create(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return nil
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
		if _, err := probeLive(t, c, cluster.IDPodSecurityAdmissionEnforced); err == nil || !strings.Contains(err.Error(), "delete scratch namespace") {
			t.Fatalf("expected cleanup deadline to make the probe indeterminate, got %v", err)
		}
	})

}

func TestAppArmorCapable(t *testing.T) {
	t.Run("every eligible Linux node must pass", func(t *testing.T) {
		workerA := readyLinuxNode("worker-a")
		workerB := readyLinuxNode("worker-b")
		workerB.Labels["kubernetes.io/hostname"] = workerA.Labels["kubernetes.io/hostname"]
		c, recorder := liveClient(t, []client.Object{workerA, workerB}, healthyLivePod)
		res, err := probeLive(t, c, cluster.IDAppArmorCapable)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass || !strings.Contains(res.Message, "worker-a, worker-b") {
			t.Fatalf("status/message = %q/%q, want all-node pass", res.Status, res.Message)
		}
		if len(recorder.pods) != 2 {
			t.Fatalf("created %d AppArmor canaries, want one per eligible node", len(recorder.pods))
		}
		targets := map[string]bool{}
		for _, pod := range recorder.pods {
			if canaryNode(pod) == "" {
				t.Errorf("canary must target an exact eligible node: %+v", pod.Spec.Affinity)
			}
			targets[canaryNode(pod)] = true
			sc := pod.Spec.Containers[0].SecurityContext
			if sc.AppArmorProfile == nil || sc.AppArmorProfile.Type != corev1.AppArmorProfileTypeLocalhost || sc.AppArmorProfile.LocalhostProfile == nil || *sc.AppArmorProfile.LocalhostProfile != "orkano-buildkit" {
				t.Errorf("canary AppArmor profile = %+v, want Localhost/orkano-buildkit", sc.AppArmorProfile)
			}
		}
		for _, name := range []string{"worker-a", "worker-b"} {
			if !targets[name] {
				t.Errorf("missing canary pinned to %s; targets were %v", name, targets)
			}
		}
		assertScratchNamespacesDeleted(t, c, recorder)
	})

	t.Run("profile mismatch on any eligible node fails", func(t *testing.T) {
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a"), readyLinuxNode("worker-b")}, func(pod *corev1.Pod) error {
			if canaryNode(pod) == "worker-b" {
				terminalPod(pod, 42)
			} else {
				terminalPod(pod, 0)
			}
			return nil
		})
		res, err := probeLive(t, c, cluster.IDAppArmorCapable)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail || !strings.Contains(res.Message, "worker-b") {
			t.Fatalf("status/message = %q/%q, want worker-b failure", res.Status, res.Message)
		}
	})

	t.Run("known AppArmor startup evidence fails rather than timing out as unknown", func(t *testing.T) {
		restore := cluster.SetLiveProbeTimingForTest(25*time.Millisecond, time.Millisecond)
		defer restore()
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a")}, func(pod *corev1.Pod) error {
			pod.Status.Phase = corev1.PodPending
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "probe",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "CreateContainerError",
					Message: "AppArmor profile not found",
				}},
			}}
			return nil
		})
		res, err := probeLive(t, c, cluster.IDAppArmorCapable)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail || !strings.Contains(res.Message, "AppArmor") {
			t.Fatalf("status/message = %q/%q, want AppArmor failure", res.Status, res.Message)
		}
	})
}

func TestSeccompDefaultDisabled(t *testing.T) {
	t.Run("fieldless canaries pass on every eligible node", func(t *testing.T) {
		c, recorder := liveClient(t, []client.Object{readyLinuxNode("worker-a"), readyLinuxNode("worker-b")}, healthyLivePod)
		res, err := probeLive(t, c, cluster.IDSeccompDefaultDisabled)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		for _, pod := range recorder.pods {
			if pod.Spec.SecurityContext.SeccompProfile != nil || pod.Spec.Containers[0].SecurityContext.SeccompProfile != nil {
				t.Errorf("seccomp canary must omit both seccomp fields: pod=%+v container=%+v", pod.Spec.SecurityContext.SeccompProfile, pod.Spec.Containers[0].SecurityContext.SeccompProfile)
			}
		}
	})

	t.Run("nonzero Seccomp mode on any node fails", func(t *testing.T) {
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a"), readyLinuxNode("worker-b")}, func(pod *corev1.Pod) error {
			if canaryNode(pod) == "worker-b" {
				terminalPod(pod, 42)
			} else {
				terminalPod(pod, 0)
			}
			return nil
		})
		res, err := probeLive(t, c, cluster.IDSeccompDefaultDisabled)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail || !strings.Contains(res.Message, "worker-b") {
			t.Fatalf("status/message = %q/%q, want worker-b failure", res.Status, res.Message)
		}
	})

	t.Run("malformed seccomp observation is indeterminate", func(t *testing.T) {
		c, _ := liveClient(t, []client.Object{readyLinuxNode("worker-a")}, func(pod *corev1.Pod) error {
			terminalPod(pod, 43)
			return nil
		})
		if _, err := probeLive(t, c, cluster.IDSeccompDefaultDisabled); err == nil || !strings.Contains(err.Error(), "could not parse") {
			t.Fatalf("expected indeterminate parsing error, got %v", err)
		}
	})
}
