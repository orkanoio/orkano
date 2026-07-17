package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// appResponse is the read DTO for an App: identity, the real spec, and the
// operator-owned status (read-only to the dashboard). It deliberately omits the
// Kubernetes plumbing (resourceVersion, managedFields, ...) the UI never needs.
type appResponse struct {
	Name              string                   `json:"name"`
	Namespace         string                   `json:"namespace"`
	CreationTimestamp metav1.Time              `json:"creationTimestamp"`
	Spec              orkanov1alpha1.AppSpec   `json:"spec"`
	Status            orkanov1alpha1.AppStatus `json:"status"`
}

func appToResponse(app *orkanov1alpha1.App) appResponse {
	return appResponse{
		Name:              app.Name,
		Namespace:         app.Namespace,
		CreationTimestamp: app.CreationTimestamp,
		Spec:              app.Spec,
		Status:            app.Status,
	}
}

// appCreateRequest is the create DTO: the caller names the App and supplies its
// spec. Status is operator-owned and not accepted (DisallowUnknownFields rejects
// a status field outright).
type appCreateRequest struct {
	Name string                 `json:"name"`
	Spec orkanov1alpha1.AppSpec `json:"spec"`
}

// appUpdateRequest replaces an App's spec (the name comes from the path). The
// dashboard writes spec only; status stays operator-owned.
type appUpdateRequest struct {
	Spec orkanov1alpha1.AppSpec `json:"spec"`
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	var list orkanov1alpha1.AppList
	if err := s.cfg.ViewerClient.List(r.Context(), &list, client.InNamespace(appsNamespace)); err != nil {
		s.writeK8sError(w, "apps.list", err)
		return
	}
	out := make([]appResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, appToResponse(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	var app orkanov1alpha1.App
	key := client.ObjectKey{Namespace: appsNamespace, Name: chi.URLParam(r, "name")}
	if err := s.cfg.ViewerClient.Get(r.Context(), key, &app); err != nil {
		s.writeK8sError(w, "apps.get", err)
		return
	}
	writeJSON(w, http.StatusOK, appToResponse(&app))
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	var req appCreateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	if !validResourceName(req.Name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name")
		return
	}
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	if kind, err := s.conflictingResourceKind(r.Context(), req.Name, "App"); err != nil {
		s.auditResult(r, user, "app.create", req.Name, err)
		s.writeK8sError(w, "apps.create", err)
		return
	} else if kind != "" {
		s.auditResult(r, user, "app.create", req.Name, errResourceNameInUse)
		writeNameInUse(w, kind)
		return
	}
	app := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: appsNamespace},
		Spec:       req.Spec,
	}
	err := s.cfg.K8s.Create(r.Context(), app)
	s.auditResult(r, user, "app.create", req.Name, err)
	if err != nil {
		s.writeK8sError(w, "apps.create", err)
		return
	}
	s.recordDeploy(r.Context(), req.Name, deployStatusCreated)
	writeJSON(w, http.StatusCreated, appToResponse(app))
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	var req appUpdateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	// Read-modify-write preserves the live object's metadata + resourceVersion (so
	// the Update gets optimistic-concurrency protection) and never reads or writes
	// the status subresource the operator owns. This Get is the read leg of an SA
	// write, NOT a user-facing read view, so it stays on the SA client — the whole
	// write path runs as the SA, and a write must never fail because the viewer
	// (impersonation) client is misconfigured.
	var app orkanov1alpha1.App
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.K8s.Get(r.Context(), key, &app); err != nil {
		// Label the log line "apps.update": the read is the first leg of an update,
		// so an operator triaging a failure should see the real operation.
		s.auditResult(r, user, "app.update", name, err)
		s.writeK8sError(w, "apps.update", err)
		return
	}
	app.Spec = req.Spec
	err := s.cfg.K8s.Update(r.Context(), &app)
	s.auditResult(r, user, "app.update", name, err)
	if err != nil {
		s.writeK8sError(w, "apps.update", err)
		return
	}
	s.recordDeploy(r.Context(), name, deployStatusUpdated)
	writeJSON(w, http.StatusOK, appToResponse(&app))
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	app := &orkanov1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace}}
	err := s.cfg.K8s.Delete(r.Context(), app)
	s.auditResult(r, user, "app.delete", name, err)
	if err != nil {
		s.writeK8sError(w, "apps.delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
