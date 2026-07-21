package doctor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/doctor"
)

// testNow is the fixed doctor clock every expiry test measures against.
var testNow = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

func probeCheck(t *testing.T, opt doctor.Options, id string) (check.Result, error) {
	t.Helper()
	if opt.Now == nil {
		opt.Now = func() time.Time { return testNow }
	}
	for _, ck := range doctor.Checks(opt) {
		if ck.ID == id {
			return ck.Probe(context.Background())
		}
	}
	t.Fatalf("check %s not registered", id)
	return check.Result{}, nil
}

// certificate builds an unstructured cert-manager.io/v1 Certificate with the
// given status fields (notAfter / renewalTime as RFC3339 strings).
func certificate(ns, name string, created time.Time, status map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetCreationTimestamp(metav1.NewTime(created))
	if status != nil {
		if err := unstructured.SetNestedMap(u.Object, status, "status"); err != nil {
			panic(err)
		}
	}
	return u
}

// healthyCert is valid for a year and not yet due for renewal.
func healthyCert(ns, name string) *unstructured.Unstructured {
	return certificate(ns, name, testNow.Add(-30*24*time.Hour), map[string]interface{}{
		"notAfter":    testNow.Add(365 * 24 * time.Hour).Format(time.RFC3339),
		"renewalTime": testNow.Add(335 * 24 * time.Hour).Format(time.RFC3339),
	})
}

func TestCertificateExpiry(t *testing.T) {
	probe := func(t *testing.T, c client.Client) (check.Result, error) {
		t.Helper()
		return probeCheck(t, doctor.Options{Client: c}, doctor.IDCertificateExpiry)
	}

	t.Run("current certificates pass and name the soonest expiry", func(t *testing.T) {
		soon := certificate("orkano-apps", "blog-example-test-tls", testNow.Add(-60*24*time.Hour), map[string]interface{}{
			"notAfter":    testNow.Add(40 * 24 * time.Hour).Format(time.RFC3339),
			"renewalTime": testNow.Add(10 * 24 * time.Hour).Format(time.RFC3339),
		})
		res, err := probe(t, fakeClient(t, healthyCert("orkano-system", "orkano-registry-tls"), soon))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "blog-example-test-tls") || !strings.Contains(res.Message, "40d") {
			t.Errorf("message %q should name the soonest-expiring cert and its runway", res.Message)
		}
	})

	t.Run("expired certificate fails", func(t *testing.T) {
		expired := certificate("orkano-apps", "gone-tls", testNow.Add(-100*24*time.Hour), map[string]interface{}{
			"notAfter": testNow.Add(-3 * 24 * time.Hour).Format(time.RFC3339),
		})
		res, err := probe(t, fakeClient(t, expired, healthyCert("orkano-system", "orkano-registry-tls")))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q, want fail", res.Status)
		}
		if !strings.Contains(res.Message, "gone-tls") || !strings.Contains(res.Message, "expired") {
			t.Errorf("message %q should name the expired cert", res.Message)
		}
	})

	t.Run("certificate inside the renewal floor fails", func(t *testing.T) {
		closeCall := certificate("orkano-apps", "late-tls", testNow.Add(-80*24*time.Hour), map[string]interface{}{
			"notAfter": testNow.Add(10 * 24 * time.Hour).Format(time.RFC3339),
		})
		res, err := probe(t, fakeClient(t, closeCall))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "expires in 10d") {
			t.Errorf("message %q should carry the remaining runway", res.Message)
		}
	})

	// The 10-year internal CA renews a year before expiry; a stuck renewal
	// must fail even though its notAfter stays far beyond the absolute floor.
	t.Run("long-lived certificate with overdue renewal fails", func(t *testing.T) {
		ca := certificate("orkano-system", "orkano-internal-ca", testNow.Add(-9*365*24*time.Hour), map[string]interface{}{
			"notAfter":    testNow.Add(300 * 24 * time.Hour).Format(time.RFC3339),
			"renewalTime": testNow.Add(-10 * 24 * time.Hour).Format(time.RFC3339),
		})
		res, err := probe(t, fakeClient(t, ca))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "renewal is overdue") || !strings.Contains(res.Message, "orkano-internal-ca") {
			t.Errorf("message %q should call out the stuck renewal", res.Message)
		}
	})

	// Let's Encrypt's duplicate-certificate rate limit can legitimately hold a
	// renewal for up to a week; only past that is renewal considered stuck.
	t.Run("renewal a few days past due is still a pass", func(t *testing.T) {
		renewing := certificate("orkano-system", "orkano-registry-tls", testNow.Add(-300*24*time.Hour), map[string]interface{}{
			"notAfter":    testNow.Add(60 * 24 * time.Hour).Format(time.RFC3339),
			"renewalTime": testNow.Add(-5 * 24 * time.Hour).Format(time.RFC3339),
		})
		res, err := probe(t, fakeClient(t, renewing))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass — renewal is in flight, not stuck", res.Status, res.Message)
		}
	})

	t.Run("young unissued certificate passes as pending", func(t *testing.T) {
		fresh := certificate("orkano-apps", "brand-new-tls", testNow.Add(-10*time.Minute), nil)
		res, err := probe(t, fakeClient(t, fresh))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "awaiting first issuance") {
			t.Errorf("message %q should note the pending issuance", res.Message)
		}
	})

	t.Run("old unissued certificate fails", func(t *testing.T) {
		stuck := certificate("orkano-apps", "never-issued-tls", testNow.Add(-2*24*time.Hour), nil)
		res, err := probe(t, fakeClient(t, stuck))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "never been issued") {
			t.Errorf("message %q should call out the failed issuance", res.Message)
		}
	})

	// Only orkano-system and orkano-apps are Orkano's TLS surface; an expired
	// cert elsewhere belongs to someone else's stack.
	t.Run("certificates outside the orkano namespaces are ignored", func(t *testing.T) {
		foreign := certificate("default", "foreign-tls", testNow.Add(-100*24*time.Hour), map[string]interface{}{
			"notAfter": testNow.Add(-1 * 24 * time.Hour).Format(time.RFC3339),
		})
		res, err := probe(t, fakeClient(t, foreign, healthyCert("orkano-system", "orkano-registry-tls")))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
	})

	// Init always creates the internal CA + registry Certificates, so zero
	// certs (or no cert-manager at all) is a broken install, never an
	// inapplicable skip that would vanish from the hardening score.
	t.Run("no certificates fails", func(t *testing.T) {
		res, err := probe(t, fakeClient(t))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "platform PKI") {
			t.Errorf("message %q should explain the missing PKI", res.Message)
		}
	})

	t.Run("absent cert-manager CRD fails", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Group == "cert-manager.io" {
						return &meta.NoKindMatchError{GroupKind: u.GroupVersionKind().GroupKind()}
					}
					return cl.List(ctx, list, opts...)
				},
			}).Build()
		res, err := probe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "TLS subsystem") {
			t.Errorf("message %q should call out the missing cert-manager install", res.Message)
		}
	})

	t.Run("list failure is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("apiserver unreachable")
				},
			}).Build()
		if _, err := probe(t, c); err == nil {
			t.Fatal("expected a probe error")
		}
	})

	t.Run("malformed notAfter is a probe error", func(t *testing.T) {
		garbled := certificate("orkano-system", "garbled-tls", testNow.Add(-time.Hour), map[string]interface{}{
			"notAfter": "not-a-timestamp",
		})
		_, err := probe(t, fakeClient(t, garbled))
		if err == nil || !strings.Contains(err.Error(), "garbled-tls") {
			t.Fatalf("expected a probe error naming the certificate, got %v", err)
		}
	})

	t.Run("malformed renewalTime is a probe error", func(t *testing.T) {
		garbled := certificate("orkano-system", "garbled-renewal-tls", testNow.Add(-time.Hour), map[string]interface{}{
			"notAfter":    testNow.Add(365 * 24 * time.Hour).Format(time.RFC3339),
			"renewalTime": "next-thursday",
		})
		_, err := probe(t, fakeClient(t, garbled))
		if err == nil || !strings.Contains(err.Error(), "garbled-renewal-tls") {
			t.Fatalf("expected a probe error naming the certificate, got %v", err)
		}
	})

	// A cert that expired minutes ago must render minutes, not "0h ago".
	t.Run("sub-hour expiry renders minutes", func(t *testing.T) {
		justExpired := certificate("orkano-apps", "just-gone-tls", testNow.Add(-90*24*time.Hour), map[string]interface{}{
			"notAfter": testNow.Add(-5 * time.Minute).Format(time.RFC3339),
		})
		res, err := probe(t, fakeClient(t, justExpired))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "expired 5m ago") {
			t.Errorf("message %q should render sub-hour ages in minutes", res.Message)
		}
	})
}
