package server

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ViewerUser and ViewerGroup are the FIXED identity the dashboard impersonates
// for every read view. Both are resourceNames-pinned in the dashboard's
// impersonate ClusterRole (ADR-0015), so there is no unpinned impersonate
// surface: a read runs as a stable, view-only identity in the cluster's RBAC +
// audit trail — never the dashboard SA — and the individual human is attributed
// in Orkano's own audit_log (INV-08). The group is bound to the read-only
// orkano-viewer Role. Both literals MUST match the resourceNames pins and the
// RoleBinding subject in config/rbac (a cross-file contract).
const (
	ViewerUser  = "orkano:viewer"
	ViewerGroup = "orkano:viewers"
)

// viewerConfig copies base and pins the impersonation to the fixed viewer
// identity. The base config is never mutated.
func viewerConfig(base *rest.Config) *rest.Config {
	cfg := rest.CopyConfig(base)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: ViewerUser,
		Groups:   []string{ViewerGroup},
		// Extra is deliberately left unset: Impersonate-Extra-* headers are NOT
		// constrained by resourceNames, so feeding any value into Extra would open
		// an unpinned impersonation surface (ADR-0013/ADR-0015).
	}
	return cfg
}

// NewViewerClient builds the read client that impersonates the fixed viewer
// identity. It is a singleton — the identity never varies, so there is no
// per-request construction and no per-user state to leak — reusing the base
// scheme + RESTMapper so it does no discovery.
func NewViewerClient(base *rest.Config, scheme *runtime.Scheme, mapper meta.RESTMapper) (client.Client, error) {
	return client.New(viewerConfig(base), client.Options{Scheme: scheme, Mapper: mapper})
}
