package server

import (
	"context"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func seedDomain(name string) *orkanov1alpha1.Domain {
	return &orkanov1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec: orkanov1alpha1.DomainSpec{
			Host:   name + ".example.test",
			AppRef: orkanov1alpha1.LocalObjectRef{Name: "demo"},
		},
		Status: orkanov1alpha1.DomainStatus{
			Conditions: []metav1.Condition{{
				Type:               orkanov1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Available",
				LastTransitionTime: metav1.NewTime(fixedNow()),
			}},
		},
	}
}

func domainCreateBody(name, host string) domainCreateRequest {
	return domainCreateRequest{
		Name: name,
		Spec: orkanov1alpha1.DomainSpec{Host: host, AppRef: orkanov1alpha1.LocalObjectRef{Name: "demo"}},
	}
}

func getDomain(t *testing.T, s *Server, name string) (orkanov1alpha1.Domain, error) {
	t.Helper()
	var d orkanov1alpha1.Domain
	err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: name}, &d)
	return d, err
}

// TestCreateDomain proves a Domain create needs only a session (it is creative,
// not destructive — step-up gates deletes and secret rotation, not provisioning).
func TestCreateDomain(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/domains", domainCreateBody("demo-example", "demo.example.test"), ck)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	got, err := getDomain(t, s, "demo-example")
	if err != nil {
		t.Fatalf("created domain not found: %v", err)
	}
	if got.Spec.Host != "demo.example.test" || got.Spec.AppRef.Name != "demo" {
		t.Fatalf("spec not stored: %+v", got.Spec)
	}
	assertAudited(t, store, "domain.create", "success")
}

func TestCreateDomainConflict(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedDomain("demo-example"))
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/domains", domainCreateBody("demo-example", "demo.example.test"), ck)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "already_exists" {
		t.Fatalf("error = %v, want already_exists", got)
	}
	assertAudited(t, store, "domain.create", "failure")
}

func TestCreateDomainInvalidName(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/domains", domainCreateBody("Bad Name", "demo.example.test"), ck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name = %d, want 400", rec.Code)
	}
}

func TestCreateDomainRequiresSession(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)

	rec := apiReq(t, s, http.MethodPost, "/api/domains", domainCreateBody("demo-example", "demo.example.test"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session = %d, want 401", rec.Code)
	}
}

func TestGetDomain(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedDomain("demo-example"))
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/domains/demo-example", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	spec, _ := body["spec"].(map[string]any)
	if spec["host"] != "demo-example.example.test" {
		t.Fatalf("spec not surfaced: %v", body["spec"])
	}
}

func TestListDomainsNamespacePinned(t *testing.T) {
	store := newFakeStore()
	elsewhere := seedDomain("elsewhere")
	elsewhere.Namespace = "other-ns"
	s := apiServer(t, store, seedDomain("a"), seedDomain("b"), elsewhere)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/domains", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	items, _ := decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (orkano-apps only)", len(items))
	}
}

func TestDeleteDomainRequiresStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedDomain("demo-example"))
	ck := authedSession(t, store)

	if rec := apiReq(t, s, http.MethodDelete, "/api/domains/demo-example", nil, ck); rec.Code != http.StatusForbidden {
		t.Fatalf("delete without step-up = %d, want 403", rec.Code)
	}
	if _, err := getDomain(t, s, "demo-example"); err != nil {
		t.Fatalf("domain deleted despite 403: %v", err)
	}

	freshenStepUp(t, store, ck.Value)
	if rec := apiReq(t, s, http.MethodDelete, "/api/domains/demo-example", nil, ck); rec.Code != http.StatusNoContent {
		t.Fatalf("delete with step-up = %d, want 204", rec.Code)
	}
	if _, err := getDomain(t, s, "demo-example"); !apierrors.IsNotFound(err) {
		t.Fatalf("domain not deleted: err=%v", err)
	}
	assertAudited(t, store, "domain.delete", "success")
}

func TestDeleteDomainNotFoundAudited(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodDelete, "/api/domains/ghost", nil, ck)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d, want 404", rec.Code)
	}
	assertAudited(t, store, "domain.delete", "failure")
}
