package server

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/internal/features"
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

// appSourceUpdateRequest is deliberately source-scoped. The handler merges it
// into the latest App so a Source tab opened before a runtime/env edit cannot
// overwrite that newer state with a stale whole-spec snapshot.
type appSourceUpdateRequest struct {
	Source orkanov1alpha1.Source        `json:"source"`
	Build  orkanov1alpha1.BuildStrategy `json:"build"`
}

var errSourceUpdateRequired = errors.New("source and build settings must be changed through the source endpoint")

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
	if err := s.cfg.Features.ValidateApp(req.Spec); err != nil {
		s.writeFeatureDisabled(w, err)
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
	if !equality.Semantic.DeepEqual(app.Spec.Source, req.Spec.Source) || !equality.Semantic.DeepEqual(app.Spec.Build, req.Spec.Build) {
		s.auditResult(r, user, "app.update", name, errSourceUpdateRequired)
		writeJSONError(w, http.StatusConflict, "source_update_required")
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

func (s *Server) handleUpdateAppSource(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	var req appSourceUpdateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}

	var app orkanov1alpha1.App
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.K8s.Get(r.Context(), key, &app); err != nil {
		s.auditResult(r, user, "app.source.update", name, err)
		s.writeK8sError(w, "apps.source.update", err)
		return
	}
	changed := !equality.Semantic.DeepEqual(app.Spec.Source, req.Source) || !equality.Semantic.DeepEqual(app.Spec.Build, req.Build)
	if changed {
		candidate := app.Spec.DeepCopy()
		candidate.Source = req.Source
		candidate.Build = req.Build
		if err := s.cfg.Features.ValidateApp(*candidate); err != nil {
			s.auditResult(r, user, "app.source.update", name, err)
			s.writeFeatureDisabled(w, err)
			return
		}
		app.Spec.Source = req.Source
		app.Spec.Build = req.Build
		if err := s.cfg.K8s.Update(r.Context(), &app); err != nil {
			s.auditResult(r, user, "app.source.update", name, err)
			s.writeK8sError(w, "apps.source.update", err)
			return
		}
	}
	s.auditResult(r, user, "app.source.update", name, nil)
	if changed {
		s.recordDeploy(r.Context(), name, deployStatusUpdated)
	}
	writeJSON(w, http.StatusOK, appToResponse(&app))
}

func (s *Server) writeFeatureDisabled(w http.ResponseWriter, err error) {
	var disabled *features.DisabledError
	if !errors.As(err, &disabled) {
		writeJSONError(w, http.StatusForbidden, "feature_disabled")
		return
	}
	ids := make([]string, len(disabled.IDs))
	for i := range disabled.IDs {
		ids[i] = string(disabled.IDs[i])
	}
	writeJSON(w, http.StatusForbidden, map[string]any{"error": "feature_disabled", "features": ids})
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
