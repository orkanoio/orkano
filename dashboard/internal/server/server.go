// Package server is the dashboard's HTTP layer: a chi router serving the
// embedded SPA plus health probes, and holding the controller-runtime client
// the dashboard uses to write Orkano custom resources (the App/catalog API lands
// in M2.4). It never holds cluster-admin — its only Kubernetes reach is the
// orkano-dashboard Role (CRUD on Orkano CRDs + value-blind Secret writes,
// INV-01/ADR-0013).
package server

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/dashboard/internal/auth"
)

// readyTimeout bounds the dependency checks /readyz performs so a wedged backend
// can never hang the probe past the kubelet's own timeout.
const readyTimeout = 2 * time.Second

// Pinger reports whether a backing store is reachable. *pgxpool.Pool satisfies
// it; tests supply a fake. An empty-statement ping needs no table privilege, so
// it works under the least-privilege orkano_dashboard role.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Config wires the server's collaborators. K8s, DB, Store, Cipher, and
// BootstrapTokenHash are required; SPA is the embedded UI tree; Logger defaults
// to a discarding logger and Now to time.Now when nil.
type Config struct {
	// K8s writes Orkano custom resources as the dashboard ServiceAccount (the
	// mutation path). Read views go through ViewerClient instead.
	K8s client.Client
	// ViewerClient reads Orkano CRs while impersonating the fixed, resourceNames-
	// pinned viewer identity (ViewerUser + ViewerGroup), so reads run as a
	// view-only identity in the cluster's RBAC + audit trail, never the dashboard
	// SA (ADR-0013/ADR-0015). It is a singleton — the identity never varies.
	// Required.
	ViewerClient client.Client
	// DB backs the /readyz probe (and, later, the dashboard's own tables).
	DB Pinger
	// Store is the dashboard's own metadata store (users, sessions, audit,
	// recovery codes) behind the bootstrap-auth flows.
	Store Store
	// Cipher encrypts the TOTP seed at rest and seals the short-lived challenge
	// cookies the auth flow uses mid-handshake.
	Cipher *auth.Cipher
	// PodLogs streams an App's pod logs for the live-logs view (SSE), through the
	// fixed viewer impersonation (ADR-0015) like every other read. Required.
	PodLogs PodLogStreamer
	// OIDC, when non-nil, enables SSO sign-in (ADR-0016). It is OPTIONAL: a nil
	// value (no issuer configured, or a misconfiguration main logged and skipped)
	// leaves OIDC disabled while the local admin keeps working — break-glass. main
	// must only set it to a usable authenticator, never a typed-nil pointer.
	OIDC OIDCAuthenticator
	// GitHub exchanges a manifest code for App credentials in the GitHub App
	// manifest flow (M2.6). OPTIONAL: New defaults it to a client against
	// api.github.com (the default is always constructable — no discovery), so
	// production never has to set it and tests supply a fake.
	GitHub ManifestExchanger
	// GitHubBaseURL is the github.com base the App-creation form POSTs to; empty
	// defaults to https://github.com. Set it for GitHub Enterprise.
	GitHubBaseURL string
	// WebhookURL is the receiver's public webhook endpoint baked into the generated
	// GitHub App manifest. The manifest flow refuses (409) without it — GitHub must
	// be able to deliver signed webhooks somewhere.
	WebhookURL string
	// PublicURL is the dashboard's external base URL used to build the manifest
	// redirect (callback) URL. Empty derives it from the request (scheme + Host),
	// which is trustworthy on the dashboard's private access paths (INV-05).
	PublicURL string
	// BootstrapTokenHash is hex(sha256(install token)); the redeem flow compares a
	// presented token's hash against it in constant time.
	BootstrapTokenHash string
	// SPA is the embedded single-page app served on every non-API path.
	SPA fs.FS
	// Logger receives structured logs; nil discards them.
	Logger *slog.Logger
	// Now is the injectable clock for session/lockout/challenge deadlines; nil
	// defaults to time.Now.
	Now func() time.Time
}

// Server is the dashboard HTTP server.
type Server struct {
	cfg    Config
	log    *slog.Logger
	router chi.Router
	rl     *rateLimiter
}

// now returns the configured clock (or time.Now).
func (s *Server) now() time.Time { return s.cfg.Now() }

// New validates the configuration and builds the router. It returns an error
// rather than panicking so main can report a clear startup failure.
func New(cfg Config) (*Server, error) {
	if cfg.K8s == nil {
		return nil, errors.New("server: K8s client is required")
	}
	if cfg.ViewerClient == nil {
		return nil, errors.New("server: ViewerClient is required")
	}
	if cfg.PodLogs == nil {
		return nil, errors.New("server: PodLogs streamer is required")
	}
	if cfg.DB == nil {
		return nil, errors.New("server: DB pinger is required")
	}
	if cfg.SPA == nil {
		return nil, errors.New("server: SPA filesystem is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("server: Store is required")
	}
	if cfg.Cipher == nil {
		return nil, errors.New("server: Cipher is required")
	}
	if cfg.BootstrapTokenHash == "" {
		return nil, errors.New("server: BootstrapTokenHash is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// The GitHub manifest exchanger is always constructable (no startup I/O), so
	// default it rather than require it — tests override with a fake. Its base is
	// the API host (api.github.com), distinct from GitHubBaseURL (the github.com
	// form host); main overrides it for GitHub Enterprise.
	if cfg.GitHub == nil {
		cfg.GitHub = NewGitHubExchanger("")
	}

	s := &Server{
		cfg: cfg,
		log: log,
		rl:  newRateLimiter(rateLimitMax, rateLimitWindow, cfg.Now),
	}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	// Liveness: process is up. Readiness: backing store reachable.
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	// Bootstrap-auth API, mounted ahead of the SPA catch-all.
	s.mountAuthRoutes(r)

	// M2.4 App/catalog API, also under /api and ahead of the SPA catch-all.
	s.mountAPIRoutes(r)

	// Everything else is the SPA (client-side routing); chi matches the /api
	// routes above ahead of this catch-all.
	r.Handle("/*", s.spaHandler())

	s.router = r
	return s, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeOK(w)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := s.cfg.DB.Ping(ctx); err != nil {
		s.log.Warn("readiness ping failed", "err", err)
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	writeOK(w)
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// spaHandler serves files from the embedded SPA tree and falls back to
// index.html for any path that is not a real file — so deep links into
// client-side routes resolve. The path is cleaned against root first, so "."
// and ".." collapse and the lookup can never escape the embedded tree.
func (s *Server) spaHandler() http.HandlerFunc {
	fileServer := http.FileServerFS(s.cfg.SPA)
	return func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean("/" + r.URL.Path)
		name := strings.TrimPrefix(clean, "/")
		if name == "" {
			s.serveIndex(w)
			return
		}
		info, err := fs.Stat(s.cfg.SPA, name)
		if err != nil || info.IsDir() {
			s.serveIndex(w)
			return
		}
		// Everything under assets/ is content-hashed by the Vite build, so it can
		// be cached forever — and should be: embedded files carry no modtime, so
		// without this header every asset would re-download on each page load.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		// Serve the canonical path so http.FileServerFS does not 301-redirect a
		// non-canonical request (which would also leak which files exist).
		r.URL.Path = clean
		fileServer.ServeHTTP(w, r)
	}
}

func (s *Server) serveIndex(w http.ResponseWriter) {
	b, err := fs.ReadFile(s.cfg.SPA, "index.html")
	if err != nil {
		s.log.Error("SPA index.html missing from embedded assets", "err", err)
		http.Error(w, "dashboard UI unavailable", http.StatusInternalServerError)
		return
	}
	// The shell must not be cached, or a deploy can serve a stale index.html that
	// references hashed asset bundles no longer present.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
