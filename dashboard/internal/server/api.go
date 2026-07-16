package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

	// Pagination defaults for the deploy-history and audit read views.
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// parsePage reads the limit/offset query params, clamping to sane bounds. An
// absent, malformed, or out-of-range value falls back to the default rather than
// erroring — a read view should not 400 on a bad page cursor.
func parsePage(r *http.Request) (limit, offset int32) {
	limit, offset = defaultPageLimit, 0
	// ParseInt with bitSize 32 range-checks into int32, so the conversions cannot
	// overflow (an out-of-range cursor just falls back to the default).
	if n, err := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 32); err == nil && n > 0 && n <= maxPageLimit {
		limit = int32(n)
	}
	if n, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 32); err == nil && n >= 0 {
		offset = int32(n)
	}
	return limit, offset
}

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
		ar.Get("/{name}/deploys", s.handleListDeploys)
		// Live logs (Server-Sent Events) — a read view, streamed through the viewer
		// impersonation like the other reads.
		ar.Get("/{name}/logs", s.handleAppLogs)
		ar.Put("/{name}", s.handleUpdateApp)
		// Destructive mutations — delete, and the env editor's secret rotation —
		// need a fresh second factor on top of the session.
		ar.Group(func(dr chi.Router) {
			dr.Use(s.RequireStepUp)
			dr.Delete("/{name}", s.handleDeleteApp)
			dr.Put("/{name}/env", s.handleSetEnv)
		})
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

	// "postgres", not the CRD plural "postgreses" (awkward English): the catalog
	// is named by its engine (ADR-0014), so the path reads as the kind.
	r.Route("/api/postgres", func(pr chi.Router) {
		pr.Use(s.RequireSession)
		pr.Get("/", s.handleListPostgres)
		pr.Post("/", s.handleCreatePostgres)
		pr.Get("/{name}/logs", s.handlePostgresLogs)
		pr.Get("/{name}", s.handleGetPostgres)
		pr.Put("/{name}", s.handleUpdatePostgres)
		// Deleting a database destroys its data (delete-and-recreate is the only
		// way to change the immutable version, ADR-0014), so it gates on step-up.
		pr.With(s.RequireStepUp).Delete("/{name}", s.handleDeletePostgres)
	})

	r.Route("/api/mongo", func(mr chi.Router) {
		mr.Use(s.RequireSession)
		mr.Get("/", s.handleListMongo)
		mr.Post("/", s.handleCreateMongo)
		mr.Get("/{name}/logs", s.handleMongoLogs)
		mr.Get("/{name}", s.handleGetMongo)
		mr.Put("/{name}", s.handleUpdateMongo)
		mr.With(s.RequireStepUp).Delete("/{name}", s.handleDeleteMongo)
	})

	// External-vault sync (ADR-0018): ESO's own kinds written directly, no
	// wrapper CR. Unlike the catalog routes, EVERY write here gates on step-up
	// (ADR-0018 decision 4 tightens the creates-need-only-a-session rule):
	// each one rewires what lands in app env, and they are rare operations.
	r.Route("/api/secretstores", func(vr chi.Router) {
		vr.Use(s.RequireSession)
		vr.Get("/", s.handleListSecretStores)
		vr.Group(func(wr chi.Router) {
			wr.Use(s.RequireStepUp)
			wr.Post("/", s.handleCreateSecretStore)
			wr.Put("/{name}", s.handleUpdateSecretStore)
			wr.Delete("/{name}", s.handleDeleteSecretStore)
		})
	})
	r.Route("/api/externalsecrets", func(vr chi.Router) {
		vr.Use(s.RequireSession)
		vr.Get("/", s.handleListExternalSecrets)
		vr.Group(func(wr chi.Router) {
			wr.Use(s.RequireStepUp)
			wr.Post("/", s.handleCreateExternalSecret)
			wr.Delete("/{name}", s.handleDeleteExternalSecret)
		})
	})

	// The append-only audit log (INV-08), readable by any authenticated session.
	r.With(s.RequireSession).Get("/api/audit", s.handleListAudit)

	// GitHub App manifest flow (M2.6). The start endpoint needs a session; the
	// callback is reached by GitHub's cross-site redirect, on which the
	// SameSite=Strict session cookie is NOT sent, so it authenticates via the
	// sealed Lax flow cookie the (RequireSession-gated) start set — mirroring OIDC.
	r.Route("/api/github/app", func(gr chi.Router) {
		gr.With(s.RequireSession).Get("/manifest", s.handleGitHubManifest)
		gr.Get("/callback", s.handleGitHubCallback)
	})

	// The onboarding wizard (M2.6): setup status (the wizard face of the shared
	// check registry) plus its two write steps. Reads and the access-mode choice
	// need a session; writing the OIDC configuration rewires who can sign in, so
	// it gates on a fresh second factor like the destructive mutations.
	r.Route("/api/setup", func(sr chi.Router) {
		sr.Use(s.RequireSession)
		sr.Get("/status", s.handleSetupStatus)
		sr.Post("/access-mode", s.handleSetAccessMode)
		sr.With(s.RequireStepUp).Post("/oidc", s.handleSetupOIDC)
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
	case meta.IsNoMatchError(err):
		// The dashboard binary knows Orkano's Go types, but discovery still needs
		// the CRDs installed in the cluster. A missing REST mapping means init has
		// not finished (or an install repaired itself only partially), not a bug in
		// the user's request.
		s.log.Warn("orkano CRDs are not established", "action", action, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "cluster_not_ready")
	default:
		s.log.Error("kubernetes api call failed", "action", action, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
	}
}
