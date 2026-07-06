package doctor_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/doctor"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	return scheme
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
}

func probeDashboard(t *testing.T, c client.Client) (check.Result, error) {
	t.Helper()
	for _, ck := range doctor.Checks(doctor.Options{Client: c}) {
		if ck.ID == doctor.IDDashboardNotPublic {
			return ck.Probe(context.Background())
		}
	}
	t.Fatalf("check %s not registered", doctor.IDDashboardNotPublic)
	return check.Result{}, nil
}

func dashboardService(svcType corev1.ServiceType) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-system", Name: "orkano-dashboard"},
		Spec:       corev1.ServiceSpec{Type: svcType},
	}
}

func ruleIngress(ns, name, service string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: "example.test",
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path: "/",
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{Name: service},
							},
						}},
					},
				},
			}},
		},
	}
}

// traefikRoute builds an unstructured traefik.io/v1alpha1 IngressRoute or
// IngressRouteTCP whose first route carries a direct Service reference.
func traefikRoute(kind, ns, name, service string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: kind})
	u.SetNamespace(ns)
	u.SetName(name)
	if err := unstructured.SetNestedSlice(u.Object, []interface{}{
		map[string]interface{}{
			"match": "Host(`dash.example.test`)",
			"services": []interface{}{
				map[string]interface{}{"name": service},
			},
		},
	}, "spec", "routes"); err != nil {
		panic(err)
	}
	return u
}

func defaultBackendIngress(ns, name, service string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{
			DefaultBackend: &networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{Name: service},
			},
		},
	}
}

func TestDashboardNotPublic(t *testing.T) {
	t.Run("ClusterIP with unrelated ingresses passes", func(t *testing.T) {
		c := fakeClient(t,
			dashboardService(corev1.ServiceTypeClusterIP),
			ruleIngress("orkano-system", "orkano-receiver", "orkano-receiver"),
			// An app in orkano-apps may legitimately route to a same-named
			// Service in its own namespace; only orkano-system routes count.
			ruleIngress("orkano-apps", "coincidence", "orkano-dashboard"),
			traefikRoute("IngressRoute", "orkano-apps", "coincidence-route", "orkano-dashboard"),
		)
		res, err := probeDashboard(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
	})

	for _, svcType := range []corev1.ServiceType{corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer} {
		t.Run("service type "+string(svcType)+" fails", func(t *testing.T) {
			res, err := probeDashboard(t, fakeClient(t, dashboardService(svcType)))
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if res.Status != check.StatusFail {
				t.Fatalf("status = %q, want fail", res.Status)
			}
			if !strings.Contains(res.Message, string(svcType)) {
				t.Errorf("message %q should name the service type", res.Message)
			}
		})
	}

	t.Run("ingress rule routing to the dashboard fails", func(t *testing.T) {
		c := fakeClient(t,
			dashboardService(corev1.ServiceTypeClusterIP),
			ruleIngress("orkano-system", "oops", "orkano-dashboard"),
		)
		res, err := probeDashboard(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q, want fail", res.Status)
		}
		if !strings.Contains(res.Message, "oops") {
			t.Errorf("message %q should name the offending Ingress", res.Message)
		}
	})

	t.Run("ingress default backend routing to the dashboard fails", func(t *testing.T) {
		c := fakeClient(t,
			dashboardService(corev1.ServiceTypeClusterIP),
			defaultBackendIngress("orkano-system", "oops-default", "orkano-dashboard"),
		)
		res, err := probeDashboard(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q, want fail", res.Status)
		}
	})

	t.Run("ClusterIP with externalIPs fails", func(t *testing.T) {
		svc := dashboardService(corev1.ServiceTypeClusterIP)
		svc.Spec.ExternalIPs = []string{"203.0.113.7"}
		res, err := probeDashboard(t, fakeClient(t, svc))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q, want fail", res.Status)
		}
		if !strings.Contains(res.Message, "203.0.113.7") {
			t.Errorf("message %q should name the external IP", res.Message)
		}
	})

	for _, kind := range []string{"IngressRoute", "IngressRouteTCP"} {
		t.Run("traefik "+kind+" routing to the dashboard fails", func(t *testing.T) {
			c := fakeClient(t,
				dashboardService(corev1.ServiceTypeClusterIP),
				traefikRoute(kind, "orkano-system", "sneaky", "orkano-dashboard"),
			)
			res, err := probeDashboard(t, c)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if res.Status != check.StatusFail {
				t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
			}
			if !strings.Contains(res.Message, "sneaky") || !strings.Contains(res.Message, kind) {
				t.Errorf("message %q should name the offending %s", res.Message, kind)
			}
		})
	}

	// A cluster without the Traefik CRDs (a non-k3s substrate) has definitively
	// no IngressRoutes — that is a pass, not an indeterminate probe.
	t.Run("absent traefik CRD is not an exposure", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(dashboardService(corev1.ServiceTypeClusterIP)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Group == "traefik.io" {
						return &meta.NoKindMatchError{GroupKind: u.GroupVersionKind().GroupKind()}
					}
					return cl.List(ctx, list, opts...)
				},
			}).Build()
		res, err := probeDashboard(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
	})

	t.Run("missing dashboard service skips", func(t *testing.T) {
		res, err := probeDashboard(t, fakeClient(t))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusSkip {
			t.Fatalf("status = %q, want skip", res.Status)
		}
	})

	// A read the cluster refuses is indeterminate — a probe error, never a
	// definitive pass or fail (unknown never counts as hardened).
	t.Run("service read failure is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("apiserver unreachable")
				},
			}).Build()
		if _, err := probeDashboard(t, c); err == nil {
			t.Fatal("expected a probe error")
		}
	})

	t.Run("ingress list failure is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(dashboardService(corev1.ServiceTypeClusterIP)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("apiserver unreachable")
				},
			}).Build()
		if _, err := probeDashboard(t, c); err == nil {
			t.Fatal("expected a probe error")
		}
	})
}

// TestChecksContract pins the shipped check metadata: IDs are permanent
// (--json/CI), severities gate the exit code.
func TestChecksContract(t *testing.T) {
	want := []struct {
		id       string
		severity check.Severity
	}{
		{"exposure.dashboard-not-public", check.SeverityCritical},
		{"tls.certificate-expiry", check.SeverityWarning},
		{"backup.etcd-snapshot-age", check.SeverityWarning},
	}
	cs := doctor.Checks(doctor.Options{})
	if len(cs) != len(want) {
		t.Fatalf("Checks() returned %d checks, want %d", len(cs), len(want))
	}
	for i, w := range want {
		c := cs[i]
		if c.ID != w.id {
			t.Errorf("check %d ID = %q, want %q", i, c.ID, w.id)
		}
		if c.Severity != w.severity {
			t.Errorf("%s severity = %q, want %q", c.ID, c.Severity, w.severity)
		}
		if len(c.Requires) != 0 {
			t.Errorf("%s: unexpected Requires %v", c.ID, c.Requires)
		}
		if c.Fix != nil {
			t.Errorf("%s: no safe automatic fix exists for a read-only cluster check; Fix must be nil", c.ID)
		}
		if c.Probe == nil || c.Summary == "" || c.Remediation == "" {
			t.Errorf("%s: Probe, Summary and Remediation must all be set", c.ID)
		}
	}
}
