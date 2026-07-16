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

func seedMongo(t *testing.T, name, size string) *orkanov1alpha1.Mongo {
	t.Helper()
	return &orkanov1alpha1.Mongo{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec:       orkanov1alpha1.MongoSpec{Version: "8.0", StorageSize: qty(t, size)},
		Status: orkanov1alpha1.MongoStatus{
			SecretName: name,
			Conditions: []metav1.Condition{{
				Type: orkanov1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Available", LastTransitionTime: metav1.NewTime(fixedNow()),
			}},
		},
	}
}

func getMongo(t *testing.T, s *Server, name string) (orkanov1alpha1.Mongo, error) {
	t.Helper()
	var mongo orkanov1alpha1.Mongo
	err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{Namespace: appsNamespace, Name: name}, &mongo)
	return mongo, err
}

func TestCreateMongo(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)
	body := mongoCreateRequest{Name: "document-db", Spec: orkanov1alpha1.MongoSpec{Version: "8.0", StorageSize: qty(t, "10Gi")}}
	rec := apiReq(t, s, http.MethodPost, "/api/mongo", body, ck)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	got, err := getMongo(t, s, "document-db")
	if err != nil || got.Spec.Version != "8.0" || got.Spec.StorageSize.String() != "10Gi" {
		t.Fatalf("created Mongo = %+v, err=%v", got, err)
	}
	if got.Status.SecretName != "" || len(got.Status.Conditions) != 0 {
		t.Fatalf("dashboard wrote operator-owned status: %+v", got.Status)
	}
	assertAudited(t, store, "mongo.create", "success")
}

func TestMongoReadAndList(t *testing.T) {
	store := newFakeStore()
	elsewhere := seedMongo(t, "elsewhere", "10Gi")
	elsewhere.Namespace = "other-ns"
	s := apiServer(t, store, seedMongo(t, "documents", "20Gi"), elsewhere)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/mongo/documents", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	status, _ := body["status"].(map[string]any)
	if status["secretName"] != "documents" {
		t.Fatalf("status not surfaced: %v", status)
	}

	rec = apiReq(t, s, http.MethodGet, "/api/mongo", nil, ck)
	items, _ := decodeBody(t, rec)["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 {
		t.Fatalf("list = %d items=%d, want namespace-pinned one", rec.Code, len(items))
	}
}

func TestUpdateMongoGrowAndRejectShrink(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedMongo(t, "documents", "10Gi"))
	ck := authedSession(t, store)

	grow := mongoUpdateRequest{Spec: orkanov1alpha1.MongoSpec{Version: "8.0", StorageSize: qty(t, "20Gi")}}
	if rec := apiReq(t, s, http.MethodPut, "/api/mongo/documents", grow, ck); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d (%s)", rec.Code, rec.Body.String())
	}
	shrink := mongoUpdateRequest{Spec: orkanov1alpha1.MongoSpec{Version: "8.0", StorageSize: qty(t, "10Gi")}}
	rec := apiReq(t, s, http.MethodPut, "/api/mongo/documents", shrink, ck)
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "storage_shrink_forbidden" {
		t.Fatalf("shrink = %d (%s), want 400 storage_shrink_forbidden", rec.Code, rec.Body.String())
	}
	got, _ := getMongo(t, s, "documents")
	if got.Spec.StorageSize.String() != "20Gi" {
		t.Fatalf("storage changed despite refused shrink: %s", got.Spec.StorageSize)
	}
}

func TestDeleteMongoRequiresStepUp(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, seedMongo(t, "documents", "10Gi"))
	ck := authedSession(t, store)
	if rec := apiReq(t, s, http.MethodDelete, "/api/mongo/documents", nil, ck); rec.Code != http.StatusForbidden {
		t.Fatalf("delete without step-up = %d, want 403", rec.Code)
	}
	freshenStepUp(t, store, ck.Value)
	if rec := apiReq(t, s, http.MethodDelete, "/api/mongo/documents", nil, ck); rec.Code != http.StatusNoContent {
		t.Fatalf("delete with step-up = %d, want 204", rec.Code)
	}
	if _, err := getMongo(t, s, "documents"); !apierrors.IsNotFound(err) {
		t.Fatalf("Mongo not deleted: %v", err)
	}
	assertAudited(t, store, "mongo.delete", "success")
}
