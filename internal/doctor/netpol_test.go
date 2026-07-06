package doctor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/doctor"
)

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

// canaryOutcomes stamps a terminal phase onto each canary the probe creates,
// playing the kubelet: the fake client runs no pods, so the Create
// interceptor decides how each connectivity attempt "went" by pod name.
func canaryOutcomes(control, denyRegistry, denyEgress corev1.PodPhase) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if pod, ok := obj.(*corev1.Pod); ok {
				switch {
				case strings.Contains(pod.Name, "-control-"):
					pod.Status.Phase = control
				case strings.Contains(pod.Name, "-deny-registry-"):
					pod.Status.Phase = denyRegistry
				case strings.Contains(pod.Name, "-deny-egress-"):
					pod.Status.Phase = denyEgress
				}
			}
			return cl.Create(ctx, obj, opts...)
		},
	}
}

func netpolFake(t *testing.T, outcomes *interceptor.Funcs, extra ...client.Object) client.Client {
	t.Helper()
	objs := append([]client.Object{registryService("10.43.0.7"), apiServerService()}, extra...)
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
		o := canaryOutcomes(corev1.PodSucceeded, corev1.PodFailed, corev1.PodFailed)
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

	// The egress leg targets the apiserver, whose ingress nothing guards: an
	// unlabeled pod connecting there means the default-deny egress itself is
	// dead — even if the registry deny leg still blocks (its ingress
	// allowlist could mask a deleted egress policy).
	t.Run("unlabeled pod reaching the apiserver fails as broken egress", func(t *testing.T) {
		o := canaryOutcomes(corev1.PodSucceeded, corev1.PodFailed, corev1.PodSucceeded)
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
	})

	t.Run("nothing enforced fails naming the egress leg", func(t *testing.T) {
		o := canaryOutcomes(corev1.PodSucceeded, corev1.PodSucceeded, corev1.PodSucceeded)
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
		o := canaryOutcomes(corev1.PodSucceeded, corev1.PodSucceeded, corev1.PodFailed)
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

	// If the ALLOWED path cannot connect, blocked deny canaries prove
	// nothing — the registry might just be down. Indeterminate, never a pass.
	t.Run("failed control leg is a probe error", func(t *testing.T) {
		o := canaryOutcomes(corev1.PodFailed, corev1.PodFailed, corev1.PodFailed)
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
		o := interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if pod, ok := obj.(*corev1.Pod); ok {
					pod.Status.Phase = corev1.PodFailed
					if strings.Contains(pod.Name, "-control-") {
						pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
							Name:  "probe",
							State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
						}}
					}
				}
				return cl.Create(ctx, obj, opts...)
			},
		}
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
		o := interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return errors.New("pods is forbidden")
			},
		}
		c := netpolFake(t, &o)
		if _, err := netpolProbe(t, c); err == nil {
			t.Fatal("expected a probe error")
		}
	})

	t.Run("leftover canaries from a crashed run are swept", func(t *testing.T) {
		leftover := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "orkano-builds",
			Name:      "orkano-doctor-netpol-deny-registry-11111",
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "orkano-doctor"},
		}}
		o := canaryOutcomes(corev1.PodSucceeded, corev1.PodFailed, corev1.PodFailed)
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
		restore := doctor.SetNetpolTimingForTest(50*time.Millisecond, 5*time.Millisecond)
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
		restore := doctor.SetNetpolTimingForTest(5*time.Second, time.Millisecond)
		defer restore()
		flaky := 4
		o := canaryOutcomes(corev1.PodSucceeded, corev1.PodFailed, corev1.PodFailed)
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
