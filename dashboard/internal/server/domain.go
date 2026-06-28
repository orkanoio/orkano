package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// domainResponse is the read DTO for a Domain. Both spec fields are immutable, so
// the UI surfaces an "edit" as delete-and-recreate; the operator-owned status
// carries Ready (incl. HostConflict) and CertificateReady.
type domainResponse struct {
	Name              string                      `json:"name"`
	Namespace         string                      `json:"namespace"`
	CreationTimestamp metav1.Time                 `json:"creationTimestamp"`
	Spec              orkanov1alpha1.DomainSpec   `json:"spec"`
	Status            orkanov1alpha1.DomainStatus `json:"status"`
}

func domainToResponse(d *orkanov1alpha1.Domain) domainResponse {
	return domainResponse{
		Name:              d.Name,
		Namespace:         d.Namespace,
		CreationTimestamp: d.CreationTimestamp,
		Spec:              d.Spec,
		Status:            d.Status,
	}
}

// domainCreateRequest names the Domain and supplies its (immutable) spec. There
// is no update DTO: re-pointing host or appRef is delete-and-recreate.
type domainCreateRequest struct {
	Name string                    `json:"name"`
	Spec orkanov1alpha1.DomainSpec `json:"spec"`
}

func (s *Server) handleListDomains(w http.ResponseWriter, r *http.Request) {
	vc, ok := s.viewerClient(w, r)
	if !ok {
		return
	}
	var list orkanov1alpha1.DomainList
	if err := vc.List(r.Context(), &list, client.InNamespace(appsNamespace)); err != nil {
		s.writeK8sError(w, "domains.list", err)
		return
	}
	out := make([]domainResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, domainToResponse(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleGetDomain(w http.ResponseWriter, r *http.Request) {
	vc, ok := s.viewerClient(w, r)
	if !ok {
		return
	}
	var d orkanov1alpha1.Domain
	key := client.ObjectKey{Namespace: appsNamespace, Name: chi.URLParam(r, "name")}
	if err := vc.Get(r.Context(), key, &d); err != nil {
		s.writeK8sError(w, "domains.get", err)
		return
	}
	writeJSON(w, http.StatusOK, domainToResponse(&d))
}

func (s *Server) handleCreateDomain(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	var req domainCreateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	if !validResourceName(req.Name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name")
		return
	}
	d := &orkanov1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: appsNamespace},
		Spec:       req.Spec,
	}
	err := s.cfg.K8s.Create(r.Context(), d)
	s.auditResult(r, user, "domain.create", req.Name, err)
	if err != nil {
		s.writeK8sError(w, "domains.create", err)
		return
	}
	writeJSON(w, http.StatusCreated, domainToResponse(d))
}

func (s *Server) handleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	d := &orkanov1alpha1.Domain{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace}}
	err := s.cfg.K8s.Delete(r.Context(), d)
	s.auditResult(r, user, "domain.delete", name, err)
	if err != nil {
		s.writeK8sError(w, "domains.delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
