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
		{"platform.components-ready", check.SeverityCritical},
		{"exposure.dashboard-not-public", check.SeverityCritical},
		{"tls.certificate-expiry", check.SeverityWarning},
		{"backup.etcd-snapshot-age", check.SeverityWarning},
		{"net.networkpolicy-enforced", check.SeverityCritical},
		{"secrets.store-health", check.SeverityWarning},
		{"features.unsafe-disabled", check.SeverityWarning},
	}
	assertContract(t, doctor.Checks(doctor.Options{}), want)
}

// TestReadOnlyChecksContract pins the dashboard doctor face's set: the same
// checks as Checks in the same order, minus the one pod-creating probe
// (net.networkpolicy-enforced). Two guards bracket the set relationship —
// ReadOnlyChecks must be a subset of Checks by ID, and the exact difference
// Checks minus ReadOnlyChecks must be exactly {net.networkpolicy-enforced} — so
// a check added to either set alone forces a deliberate decision about whether
// the dashboard face should run it.
func TestReadOnlyChecksContract(t *testing.T) {
	want := []struct {
		id       string
		severity check.Severity
	}{
		{"platform.components-ready", check.SeverityCritical},
		{"exposure.dashboard-not-public", check.SeverityCritical},
		{"tls.certificate-expiry", check.SeverityWarning},
		{"backup.etcd-snapshot-age", check.SeverityWarning},
		{"secrets.store-health", check.SeverityWarning},
		{"features.unsafe-disabled", check.SeverityWarning},
	}
	ro := doctor.ReadOnlyChecks(doctor.Options{})
	assertContract(t, ro, want)

	for _, c := range ro {
		if c.ID == "net.networkpolicy-enforced" {
			t.Errorf("ReadOnlyChecks must not include the pod-creating netpol probe")
		}
	}

	full := map[string]bool{}
	for _, c := range doctor.Checks(doctor.Options{}) {
		full[c.ID] = true
	}
	for _, c := range ro {
		if !full[c.ID] {
			t.Errorf("ReadOnlyChecks has %q, which Checks does not — the read-only set must be a subset", c.ID)
		}
	}

	// The subset guard above cannot fire when a check is added to Checks() alone;
	// pin the exact difference so that case is caught too — everything Checks has
	// that ReadOnlyChecks lacks must be exactly the one deliberate CLI-only probe.
	roIDs := map[string]bool{}
	for _, c := range ro {
		roIDs[c.ID] = true
	}
	var cliOnly []string
	for _, c := range doctor.Checks(doctor.Options{}) {
		if !roIDs[c.ID] {
			cliOnly = append(cliOnly, c.ID)
		}
	}
	for _, id := range cliOnly {
		if id != "net.networkpolicy-enforced" {
			t.Errorf("check %q is in Checks but not ReadOnlyChecks — either add it to ReadOnlyChecks or pin it as a deliberate CLI-only exclusion here", id)
		}
	}
	if len(cliOnly) != 1 {
		t.Errorf("Checks minus ReadOnlyChecks = %v, want exactly [net.networkpolicy-enforced]", cliOnly)
	}
}

func assertContract(t *testing.T, cs []check.Check, want []struct {
	id       string
	severity check.Severity
}) {
	t.Helper()
	if len(cs) != len(want) {
		t.Fatalf("got %d checks, want %d", len(cs), len(want))
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
			t.Errorf("%s: none of the doctor cluster checks ships a safe automatic fix; Fix must be nil", c.ID)
		}
		if c.Probe == nil || c.Summary == "" || c.Remediation == "" {
			t.Errorf("%s: Probe, Summary and Remediation must all be set", c.ID)
		}
	}
}
