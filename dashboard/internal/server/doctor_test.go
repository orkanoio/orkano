package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// --- dashboard doctor face harness ---

func doctorService(svcType corev1.ServiceType) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-system", Name: "orkano-dashboard"},
		Spec:       corev1.ServiceSpec{Type: svcType},
	}
}

func doctorDeployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-system", Name: name},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: strings.TrimPrefix(name, "orkano-"),
				Env:  []corev1.EnvVar{{Name: "ORKANO_UNSAFE_FEATURES", Value: ""}},
			}},
		}}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
}

func doctorStatefulSet(name string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-system", Name: name},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
}

// doctorHealthyCert keeps the tls.certificate-expiry check passing (a real
// install always has the platform PKI); measured against fixedNow, the server's
// injected clock, which is also doctor.Options.Now.
func doctorHealthyCert() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	u.SetNamespace("orkano-system")
	u.SetName("orkano-registry-tls")
	if err := unstructured.SetNestedMap(u.Object, map[string]interface{}{
		"notAfter":    fixedNow().Add(300 * 24 * time.Hour).Format(time.RFC3339),
		"renewalTime": fixedNow().Add(270 * 24 * time.Hour).Format(time.RFC3339),
	}, "status"); err != nil {
		panic(err)
	}
	return u
}

// doctorHealthyObjects is a fully healthy cluster for the read-only doctor face:
// a ClusterIP dashboard Service, the ready control-plane workloads, and the
// platform PKI. svcType flips the dashboard Service type to drive the exposure
// check.
func doctorHealthyObjects(svcType corev1.ServiceType) []client.Object {
	return []client.Object{
		doctorService(svcType),
		doctorDeployment("orkano-operator"),
		doctorDeployment("orkano-receiver"),
		doctorDeployment("orkano-registry"),
		doctorDeployment("orkano-dashboard"),
		doctorStatefulSet("orkano-postgres"),
		doctorHealthyCert(),
	}
}

// doctorViewer builds the impersonated-viewer client the handler reads through.
// The traefik/k3s/ESO CRDs are absent on a default install, so their list calls
// return NoMatch — the exposure check treats Traefik as absent and the backup
// and store-health checks skip (the internal/doctor test idiom).
func doctorViewer(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return doctorViewerNoMatch(t, []string{"traefik.io", "k3s.cattle.io", "external-secrets.io"}, objs...)
}

// doctorViewerNoMatch is doctorViewer with the CRD-absent group set spelled out,
// so a test can, for instance, keep ESO present (to exercise store-health) while
// traefik and k3s stay absent.
func doctorViewerNoMatch(t *testing.T, noMatchGroups []string, objs ...client.Object) client.Client {
	t.Helper()
	absent := make(map[string]bool, len(noMatchGroups))
	for _, g := range noMatchGroups {
		absent[g] = true
	}
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if u, ok := list.(*unstructured.UnstructuredList); ok && absent[u.GroupVersionKind().Group] {
					return &meta.NoKindMatchError{GroupKind: u.GroupVersionKind().GroupKind()}
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
}

// doctorESOObject builds an unstructured external-secrets.io/v1 object with a
// Ready condition, mirroring internal/doctor/secrets_test.go's esoObject.
func doctorESOObject(kind, name, ready string, extra func(map[string]interface{})) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "external-secrets.io/v1",
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": "orkano-apps"},
		"spec":       map[string]interface{}{},
	}}
	if ready != "" {
		u.Object["status"] = map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": ready, "reason": "r", "message": "m"},
			},
		}
	}
	if extra != nil {
		extra(u.Object)
	}
	return u
}

func emptyK8s(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
}

// doctorReadReport issues an authenticated GET /api/doctor and decodes the body.
func doctorReadReport(t *testing.T, s *Server, ck *http.Cookie) (int, doctorReport) {
	t.Helper()
	rec := apiReq(t, s, http.MethodGet, "/api/doctor", nil, ck)
	var rep doctorReport
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("body is not JSON: %v\n%s", err, rec.Body.String())
		}
	}
	return rec.Code, rep
}

func TestDoctorFace(t *testing.T) {
	readOnlyOrder := []string{
		"platform.components-ready",
		"exposure.dashboard-not-public",
		"tls.certificate-expiry",
		"backup.etcd-snapshot-age",
		"secrets.store-health",
		"features.unsafe-disabled",
	}

	t.Run("healthy cluster", func(t *testing.T) {
		store := newFakeStore()
		viewer := doctorViewer(t, doctorHealthyObjects(corev1.ServiceTypeClusterIP)...)
		s := serverWithViewer(t, store, emptyK8s(t), viewer)
		ck := authedSession(t, store)

		code, rep := doctorReadReport(t, s, ck)
		if code != http.StatusOK {
			t.Fatalf("GET /api/doctor = %d, want 200", code)
		}
		if rep.Status != "healthy" {
			t.Errorf("status = %q, want healthy", rep.Status)
		}
		if rep.Score.Value != 100 {
			t.Errorf("score.value = %d, want 100", rep.Score.Value)
		}
		if len(rep.Checks) != len(readOnlyOrder) {
			t.Fatalf("got %d checks, want %d (the read-only set, no netpol)", len(rep.Checks), len(readOnlyOrder))
		}
		for i, want := range readOnlyOrder {
			if rep.Checks[i].ID != want {
				t.Errorf("check %d = %q, want %q", i, rep.Checks[i].ID, want)
			}
		}
		if rep.CheckedAt == "" {
			t.Error("checkedAt should be set")
		}
	})

	t.Run("no session is 401", func(t *testing.T) {
		s := serverWithViewer(t, newFakeStore(), emptyK8s(t), emptyK8s(t))
		if rec := apiReq(t, s, http.MethodGet, "/api/doctor", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated GET = %d, want 401", rec.Code)
		}
	})

	t.Run("exposed dashboard is unhealthy with actionable detail", func(t *testing.T) {
		store := newFakeStore()
		viewer := doctorViewer(t, doctorHealthyObjects(corev1.ServiceTypeLoadBalancer)...)
		s := serverWithViewer(t, store, emptyK8s(t), viewer)
		ck := authedSession(t, store)

		code, rep := doctorReadReport(t, s, ck)
		if code != http.StatusOK {
			t.Fatalf("GET /api/doctor = %d, want 200", code)
		}
		if rep.Status != "unhealthy" {
			t.Fatalf("status = %q, want unhealthy", rep.Status)
		}
		var exposure *setupCheckJSON
		for i := range rep.Checks {
			if rep.Checks[i].ID == "exposure.dashboard-not-public" {
				exposure = &rep.Checks[i]
			}
		}
		if exposure == nil {
			t.Fatal("exposure check missing from the report")
		}
		if exposure.Outcome != "fail" || exposure.Message == "" || exposure.Remediation == "" {
			t.Errorf("exposure check should carry a fail outcome, message, and remediation: %+v", exposure)
		}
	})

	// A read the cluster refuses turns a critical check indeterminate, never a
	// definitive pass or fail — the status reflects it.
	t.Run("read error is indeterminate", func(t *testing.T) {
		store := newFakeStore()
		viewer := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("apiserver unreachable")
				},
			}).Build()
		s := serverWithViewer(t, store, emptyK8s(t), viewer)
		ck := authedSession(t, store)

		code, rep := doctorReadReport(t, s, ck)
		if code != http.StatusOK {
			t.Fatalf("GET /api/doctor = %d, want 200", code)
		}
		if rep.Status != "indeterminate" {
			t.Fatalf("status = %q, want indeterminate", rep.Status)
		}
	})

	// The handler reads through the viewer client, never the SA client: the
	// healthy fixture is seeded only into the viewer, while the SA client holds
	// an exposed dashboard Service. A run that read the SA client would report
	// unhealthy; reading the viewer, it is healthy (the TestReadsUseViewerClient
	// idiom).
	t.Run("reads use the viewer client", func(t *testing.T) {
		store := newFakeStore()
		viewer := doctorViewer(t, doctorHealthyObjects(corev1.ServiceTypeClusterIP)...)
		saClient := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(doctorService(corev1.ServiceTypeLoadBalancer)).Build()
		s := serverWithViewer(t, store, saClient, viewer)
		ck := authedSession(t, store)

		code, rep := doctorReadReport(t, s, ck)
		if code != http.StatusOK {
			t.Fatalf("GET /api/doctor = %d, want 200", code)
		}
		if rep.Status != "healthy" {
			t.Fatalf("status = %q, want healthy — the handler must read the viewer client, not the SA client", rep.Status)
		}
	})

	// With ESO present, store-health actually runs (not skipped) — and the
	// handler wires SkipSecretReads, so a Ready, fresh sync passes even though no
	// target Secret exists to read. Drop that wiring and the missing target would
	// fail the check, so this case is what makes SkipSecretReads:true load-bearing
	// in the server tests (every other case NoMatches ESO away).
	t.Run("store-health runs value-blind when ESO is present", func(t *testing.T) {
		store := newFakeStore()
		objs := doctorHealthyObjects(corev1.ServiceTypeClusterIP)
		objs = append(objs,
			doctorESOObject("SecretStore", "team-vault", "True", nil),
			doctorESOObject("ExternalSecret", "api-stripe", "True", func(o map[string]interface{}) {
				o["spec"] = map[string]interface{}{
					"refreshInterval": "1h",
					"target":          map[string]interface{}{"name": "api-stripe"},
				}
				o["status"].(map[string]interface{})["refreshTime"] = fixedNow().Add(-10 * time.Minute).Format(time.RFC3339)
			}),
		)
		// ESO stays reachable; only traefik and k3s report absent. No target
		// Secret is seeded — a value-blind run must not fail on the unread target.
		viewer := doctorViewerNoMatch(t, []string{"traefik.io", "k3s.cattle.io"}, objs...)
		s := serverWithViewer(t, store, emptyK8s(t), viewer)
		ck := authedSession(t, store)

		code, rep := doctorReadReport(t, s, ck)
		if code != http.StatusOK {
			t.Fatalf("GET /api/doctor = %d, want 200", code)
		}
		if rep.Status != "healthy" || rep.Score.Value != 100 {
			t.Fatalf("status=%q score=%d, want healthy/100 — SkipSecretReads must let the unread missing target pass", rep.Status, rep.Score.Value)
		}
	})
}
