package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// mountAPIRoutes registers the M2.4 App/catalog API under /api, as siblings of
// the /api/auth subtree. It must be called before the SPA catch-all so chi
// matches these ahead of "/*".
//
// Two middleware tiers gate the API: every route requires a valid session
// (RequireSession); destructive mutations — delete, secret rotation —
// additionally require a freshly re-proved second factor (RequireStepUp, which
// resolves the session itself and so subsumes RequireSession). The
// App/Domain/Postgres, env-editor, deploy-history, and audit handlers register
// into these tiers as their sub-commits land; the skeleton routes below prove the
// wiring and ordering until then.
func (s *Server) mountAPIRoutes(r chi.Router) {
	r.With(s.RequireSession).Get("/api/skeleton", s.handleAPINotImplemented)
	r.With(s.RequireStepUp).Post("/api/skeleton", s.handleAPINotImplemented)

	// Any other /api path is a JSON 404, never the SPA shell — an API client must
	// not receive HTML for an unknown endpoint. This pattern is more specific than
	// the root "/*" SPA catch-all, so chi matches it for unmatched /api paths only.
	r.HandleFunc("/api/*", s.handleAPINotFound)
}

// handleAPINotImplemented is the placeholder the skeleton routes serve until the
// real resource handlers land; it is replaced as the App/catalog handlers arrive.
func (s *Server) handleAPINotImplemented(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusNotImplemented, "not_implemented")
}

// handleAPINotFound answers any /api path with no registered handler.
func (s *Server) handleAPINotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusNotFound, "not_found")
}

// writeK8sError maps a Kubernetes API error onto the dashboard's snake_code JSON
// error vocabulary and an HTTP status. It surfaces only a stable code to the
// client — never the apiserver's raw message, which can name field values or
// internal detail. Forbidden, transient-unavailability, and unrecognized errors
// are logged server-side (the expected client errors — not-found, conflict,
// validation — are not, to keep the log signal high). An unrecognized error is a
// 500. The action label gives the log line context.
func (s *Server) writeK8sError(w http.ResponseWriter, action string, err error) {
	switch {
	case apierrors.IsNotFound(err):
		writeJSONError(w, http.StatusNotFound, "not_found")
	case apierrors.IsAlreadyExists(err):
		writeJSONError(w, http.StatusConflict, "already_exists")
	case apierrors.IsConflict(err):
		// Optimistic-concurrency conflict (stale resourceVersion on update).
		writeJSONError(w, http.StatusConflict, "conflict")
	case apierrors.IsInvalid(err):
		// Schema/CEL validation, including immutability rejections. The apiserver
		// message names the failing rule but can echo field values, so return a
		// stable code and keep the detail in the server log.
		writeJSONError(w, http.StatusUnprocessableEntity, "invalid")
	case apierrors.IsBadRequest(err):
		writeJSONError(w, http.StatusBadRequest, "bad_request")
	case apierrors.IsForbidden(err):
		// The dashboard SA should hold every grant it needs, so a 403 signals an
		// RBAC misconfiguration worth surfacing in the log.
		s.log.Warn("kubernetes api call forbidden", "action", action, "err", err)
		writeJSONError(w, http.StatusForbidden, "forbidden")
	case apierrors.IsUnauthorized(err):
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
	case apierrors.IsServiceUnavailable(err), apierrors.IsServerTimeout(err),
		apierrors.IsTimeout(err), apierrors.IsTooManyRequests(err):
		// Transient cluster unavailability — log so intermittent 503s leave a trace.
		s.log.Warn("kubernetes api call unavailable", "action", action, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
	default:
		s.log.Error("kubernetes api call failed", "action", action, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
	}
}
