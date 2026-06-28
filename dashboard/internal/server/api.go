package server

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// apiMaxBodyBytes bounds an /api request body. App/Postgres specs are larger
	// than the tiny auth payloads (maxAuthBodyBytes) but still small; 256 KiB is a
	// generous ceiling that rejects an accidental or hostile oversized body.
	apiMaxBodyBytes = 256 << 10

	// appsNamespace is the single namespace the orkano-dashboard Role grants CRD
	// access in; every App/Domain/Postgres the API touches lives here.
	appsNamespace = "orkano-apps"
)

// mountAPIRoutes registers the M2.4 App/catalog API under /api, as siblings of
// the /api/auth subtree. It must be called before the SPA catch-all so chi
// matches these ahead of "/*".
//
// Two middleware tiers gate the API: every route requires a valid session
// (RequireSession); destructive mutations — delete, secret rotation —
// additionally require a freshly re-proved second factor (RequireStepUp, which
// resolves the session itself and so subsumes RequireSession). Creates and
// non-destructive updates need only a session, matching ADR-0003's "step-up for
// destructive actions" (delete an app, rotate secrets), not for provisioning.
func (s *Server) mountAPIRoutes(r chi.Router) {
	r.Route("/api/apps", func(ar chi.Router) {
		ar.Use(s.RequireSession)
		ar.Get("/", s.handleListApps)
		ar.Post("/", s.handleCreateApp)
		ar.Get("/{name}", s.handleGetApp)
		ar.Put("/{name}", s.handleUpdateApp)
		ar.With(s.RequireStepUp).Delete("/{name}", s.handleDeleteApp)
	})

	r.Route("/api/domains", func(dr chi.Router) {
		dr.Use(s.RequireSession)
		dr.Get("/", s.handleListDomains)
		dr.Post("/", s.handleCreateDomain)
		dr.Get("/{name}", s.handleGetDomain)
		// Domain spec is fully immutable (host + appRef are CEL self==oldSelf), so
		// there is no update — an "edit" is delete-and-recreate. Deletion is the one
		// destructive mutation and gates on a fresh second factor.
		dr.With(s.RequireStepUp).Delete("/{name}", s.handleDeleteDomain)
	})

	// Any other /api path is a JSON 404, never the SPA shell — an API client must
	// not receive HTML for an unknown endpoint. This pattern is more specific than
	// the root "/*" SPA catch-all, so chi matches it for unmatched /api paths only.
	r.HandleFunc("/api/*", s.handleAPINotFound)
}

// handleAPINotFound answers any /api path with no registered handler.
func (s *Server) handleAPINotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusNotFound, "not_found")
}

// decodeAPIJSON decodes an /api request body into dst (a larger cap than the
// auth bodies), rejecting unknown fields so a stray field — e.g. an attempt to
// set the operator-owned status — is a clean 400.
func (s *Server) decodeAPIJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeJSONLimit(w, r, dst, apiMaxBodyBytes)
}

// decodeJSONLimit reads a bounded JSON body into dst, rejecting unknown fields.
// It writes a 400 and returns false on a read/parse error. Shared by the auth
// and API decoders, which differ only in their body cap.
func decodeJSONLimit(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

// auditResult records the outcome of a privileged mutation (INV-08): success
// when err is nil, failure otherwise. The actor is the authenticated username;
// the detail (in s.audit) carries only the client IP — never request payload.
func (s *Server) auditResult(r *http.Request, user *sessionUser, action, target string, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	s.audit(r.Context(), actorName(user), action, target, outcome, r)
}

// validResourceName reports whether name is a usable CRD object name (a DNS-1123
// subdomain). A clean client-side check returns a 400 with a stable code instead
// of a noisier apiserver rejection; the apiserver (and, for Postgres, the
// reconciler's stricter DNS-1035 check) remains the authority.
func validResourceName(name string) bool {
	return len(validation.IsDNS1123Subdomain(name)) == 0
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
