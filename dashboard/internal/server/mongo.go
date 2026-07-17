package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

type mongoResponse struct {
	Name              string                     `json:"name"`
	Namespace         string                     `json:"namespace"`
	CreationTimestamp metav1.Time                `json:"creationTimestamp"`
	Spec              orkanov1alpha1.MongoSpec   `json:"spec"`
	Status            orkanov1alpha1.MongoStatus `json:"status"`
}

func mongoToResponse(m *orkanov1alpha1.Mongo) mongoResponse {
	return mongoResponse{
		Name: m.Name, Namespace: m.Namespace, CreationTimestamp: m.CreationTimestamp, Spec: m.Spec, Status: m.Status,
	}
}

type mongoCreateRequest struct {
	Name string                   `json:"name"`
	Spec orkanov1alpha1.MongoSpec `json:"spec"`
}

type mongoUpdateRequest struct {
	Spec orkanov1alpha1.MongoSpec `json:"spec"`
}

func (s *Server) handleListMongo(w http.ResponseWriter, r *http.Request) {
	var list orkanov1alpha1.MongoList
	if err := s.cfg.ViewerClient.List(r.Context(), &list, client.InNamespace(appsNamespace)); err != nil {
		s.writeK8sError(w, "mongo.list", err)
		return
	}
	out := make([]mongoResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, mongoToResponse(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleGetMongo(w http.ResponseWriter, r *http.Request) {
	var mongo orkanov1alpha1.Mongo
	key := client.ObjectKey{Namespace: appsNamespace, Name: chi.URLParam(r, "name")}
	if err := s.cfg.ViewerClient.Get(r.Context(), key, &mongo); err != nil {
		s.writeK8sError(w, "mongo.get", err)
		return
	}
	writeJSON(w, http.StatusOK, mongoToResponse(&mongo))
}

func (s *Server) handleCreateMongo(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	var req mongoCreateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	if !validResourceName(req.Name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name")
		return
	}
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	if kind, err := s.conflictingResourceKind(r.Context(), req.Name, "Mongo"); err != nil {
		s.auditResult(r, user, "mongo.create", req.Name, err)
		s.writeK8sError(w, "mongo.create", err)
		return
	} else if kind != "" {
		s.auditResult(r, user, "mongo.create", req.Name, errResourceNameInUse)
		writeNameInUse(w, kind)
		return
	}
	if taken, err := s.esoClaimsSecretName(r.Context(), req.Name); err != nil {
		s.auditResult(r, user, "mongo.create", req.Name, err)
		s.writeK8sError(w, "mongo.create", err)
		return
	} else if taken {
		s.auditResult(r, user, "mongo.create", req.Name, errResourceNameInUse)
		writeJSONError(w, http.StatusConflict, "name_conflict")
		return
	}

	mongo := &orkanov1alpha1.Mongo{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: appsNamespace},
		Spec:       req.Spec,
	}
	err := s.cfg.K8s.Create(r.Context(), mongo)
	s.auditResult(r, user, "mongo.create", req.Name, err)
	if err != nil {
		s.writeK8sError(w, "mongo.create", err)
		return
	}
	writeJSON(w, http.StatusCreated, mongoToResponse(mongo))
}

func (s *Server) handleUpdateMongo(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	var req mongoUpdateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	var mongo orkanov1alpha1.Mongo
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.K8s.Get(r.Context(), key, &mongo); err != nil {
		s.auditResult(r, user, "mongo.update", name, err)
		s.writeK8sError(w, "mongo.update", err)
		return
	}
	if req.Spec.StorageSize == nil {
		req.Spec.StorageSize = mongo.Spec.StorageSize
	} else if mongo.Spec.StorageSize != nil && req.Spec.StorageSize.Cmp(*mongo.Spec.StorageSize) < 0 {
		writeJSONError(w, http.StatusBadRequest, "storage_shrink_forbidden")
		return
	}
	mongo.Spec = req.Spec
	err := s.cfg.K8s.Update(r.Context(), &mongo)
	s.auditResult(r, user, "mongo.update", name, err)
	if err != nil {
		s.writeK8sError(w, "mongo.update", err)
		return
	}
	writeJSON(w, http.StatusOK, mongoToResponse(&mongo))
}

func (s *Server) handleDeleteMongo(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	mongo := &orkanov1alpha1.Mongo{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace}}
	err := s.cfg.K8s.Delete(r.Context(), mongo)
	s.auditResult(r, user, "mongo.delete", name, err)
	if err != nil {
		s.writeK8sError(w, "mongo.delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
