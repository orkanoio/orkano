package server

import (
	"net/http"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ViewerGroup is the fixed Kubernetes group the dashboard impersonates for every
// read view. RBAC pins the dashboard SA's impersonate grant to exactly this group
// via resourceNames (ADR-0013), and the group is bound to the read-only
// orkano-viewer Role — so a read runs under the human's identity in the cluster
// audit trail and RBAC, never the SA, while an arbitrary Impersonate-User still
// cannot escalate beyond this group's view-only access (the group is the only
// load-bearing identity). The literal MUST match the resourceNames pin in
// config/rbac and the RoleBinding subject (a cross-file contract, landed with the
// grant in a later sub-commit).
const ViewerGroup = "orkano:viewers"

// viewerConfig copies base and sets the impersonation: the fixed viewer group
// (the load-bearing RBAC identity) plus the human's username (for the cluster
// audit trail). The base config is never mutated.
func viewerConfig(base *rest.Config, username string) *rest.Config {
	cfg := rest.CopyConfig(base)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: username,
		Groups:   []string{ViewerGroup},
		// Extra is deliberately left unset: Impersonate-Extra-* headers are NOT
		// constrained by the resourceNames pin, so feeding session-derived values
		// into Extra would open an unpinned impersonation surface (ADR-0013).
	}
	return cfg
}

// NewViewerClient builds a per-request client impersonating the viewer group as
// username. It reuses the base scheme + RESTMapper, so no per-call discovery
// happens — only a lightweight REST client is constructed.
func NewViewerClient(base *rest.Config, scheme *runtime.Scheme, mapper meta.RESTMapper, username string) (client.Client, error) {
	return client.New(viewerConfig(base, username), client.Options{Scheme: scheme, Mapper: mapper})
}

// viewerClient builds the impersonating read client for the request's user. On a
// build failure it writes a 500 and returns ok=false. Read views route through
// this so the cluster sees the human identity (ADR-0013); writes stay on the SA
// client (s.cfg.K8s).
func (s *Server) viewerClient(w http.ResponseWriter, r *http.Request) (client.Client, bool) {
	user, ok := userFromContext(r.Context())
	if !ok {
		// Defence in depth: every read view is RequireSession-gated, so the user is
		// always set here; this guards against a future route wired without it.
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	vc, err := s.cfg.ViewerClient(user.Username)
	if err != nil {
		s.log.Error("build viewer client failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return nil, false
	}
	return vc, true
}
