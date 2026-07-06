package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// postgresResponse is the read DTO for a catalog Postgres: identity, the spec
// (version + storageSize), and the operator-owned status (Ready + the produced
// connection-Secret name — never a value, INV-03).
type postgresResponse struct {
	Name              string                        `json:"name"`
	Namespace         string                        `json:"namespace"`
	CreationTimestamp metav1.Time                   `json:"creationTimestamp"`
	Spec              orkanov1alpha1.PostgresSpec   `json:"spec"`
	Status            orkanov1alpha1.PostgresStatus `json:"status"`
}

func postgresToResponse(p *orkanov1alpha1.Postgres) postgresResponse {
	return postgresResponse{
		Name:              p.Name,
		Namespace:         p.Namespace,
		CreationTimestamp: p.CreationTimestamp,
		Spec:              p.Spec,
		Status:            p.Status,
	}
}

type postgresCreateRequest struct {
	Name string                      `json:"name"`
	Spec orkanov1alpha1.PostgresSpec `json:"spec"`
}

type postgresUpdateRequest struct {
	Spec orkanov1alpha1.PostgresSpec `json:"spec"`
}

func (s *Server) handleListPostgres(w http.ResponseWriter, r *http.Request) {
	var list orkanov1alpha1.PostgresList
	if err := s.cfg.ViewerClient.List(r.Context(), &list, client.InNamespace(appsNamespace)); err != nil {
		s.writeK8sError(w, "postgres.list", err)
		return
	}
	out := make([]postgresResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, postgresToResponse(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleGetPostgres(w http.ResponseWriter, r *http.Request) {
	var p orkanov1alpha1.Postgres
	key := client.ObjectKey{Namespace: appsNamespace, Name: chi.URLParam(r, "name")}
	if err := s.cfg.ViewerClient.Get(r.Context(), key, &p); err != nil {
		s.writeK8sError(w, "postgres.get", err)
		return
	}
	writeJSON(w, http.StatusOK, postgresToResponse(&p))
}

func (s *Server) handleCreatePostgres(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	var req postgresCreateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	if !validResourceName(req.Name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name")
		return
	}
	// The connection Secret is named after this object (ADR-0014), so a name
	// an ESO sync target or a store's credentials Secret already claims would
	// collide at the Secret layer — the reconciler would refuse it onto
	// ProvisionFailed; refuse earlier with a clean 409 (ADR-0018, the mirror
	// of the vault API's collision checks).
	if taken, err := s.esoClaimsSecretName(r.Context(), req.Name); err != nil {
		s.writeK8sError(w, "postgres.create", err)
		return
	} else if taken {
		writeJSONError(w, http.StatusConflict, "name_conflict")
		return
	}
	p := &orkanov1alpha1.Postgres{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: appsNamespace},
		Spec:       req.Spec,
	}
	err := s.cfg.K8s.Create(r.Context(), p)
	s.auditResult(r, user, "postgres.create", req.Name, err)
	if err != nil {
		s.writeK8sError(w, "postgres.create", err)
		return
	}
	writeJSON(w, http.StatusCreated, postgresToResponse(p))
}

func (s *Server) handleUpdatePostgres(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	var req postgresUpdateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	// Read-modify-write on the SA client (the write path); never the viewer.
	var p orkanov1alpha1.Postgres
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.K8s.Get(r.Context(), key, &p); err != nil {
		s.auditResult(r, user, "postgres.update", name, err)
		s.writeK8sError(w, "postgres.update", err)
		return
	}
	// storageSize is grow-only, but the apiserver does NOT enforce it (native PVC
	// semantics, ADR-0014) — only the reconciler does, onto Ready=ProvisionFailed,
	// which is the backstop. Refuse a shrink here for a clean 400; an omitted size
	// preserves the current value rather than letting the schema default re-shrink
	// it. A stored object's storageSize is non-nil (the apiserver defaults it at
	// create), so the nil short-circuit below only skips the compare in tests,
	// where the fake client applies no defaults. Version immutability stays the
	// apiserver's job (a change passes through as a 422).
	if req.Spec.StorageSize == nil {
		req.Spec.StorageSize = p.Spec.StorageSize
	} else if p.Spec.StorageSize != nil && req.Spec.StorageSize.Cmp(*p.Spec.StorageSize) < 0 {
		writeJSONError(w, http.StatusBadRequest, "storage_shrink_forbidden")
		return
	}
	p.Spec = req.Spec
	err := s.cfg.K8s.Update(r.Context(), &p)
	s.auditResult(r, user, "postgres.update", name, err)
	if err != nil {
		s.writeK8sError(w, "postgres.update", err)
		return
	}
	writeJSON(w, http.StatusOK, postgresToResponse(&p))
}

func (s *Server) handleDeletePostgres(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	p := &orkanov1alpha1.Postgres{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace}}
	err := s.cfg.K8s.Delete(r.Context(), p)
	s.auditResult(r, user, "postgres.delete", name, err)
	if err != nil {
		s.writeK8sError(w, "postgres.delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
