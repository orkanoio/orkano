// Package cluster holds the BYO-cluster install preflight — the check
// framework's install face pointed at an existing cluster (ADR-0019), and the
// fourth api/check consumer after the node preflight, the onboarding wizard,
// and orkano doctor. Stage-1 installs control the whole substrate; on a
// bring-your-own cluster everything orkano init quietly guarantees is
// variable, so these checks read the cluster's actual capabilities through
// the same kubeconfig identity that will run the install.
package cluster

import (
	"fmt"

	"k8s.io/apimachinery/pkg/version"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/checks"
)

// The Orkano namespaces the install writes into; the RBAC walk probes each.
// Duplicated from the operator's cache.go on purpose — this package must not
// import operator internals (mirrors internal/doctor).
const (
	systemNamespace = "orkano-system"
	appsNamespace   = "orkano-apps"
	buildNamespace  = "orkano-builds"

	// certManagerNamespace is where the vendored cert-manager deploys when
	// certManager.install is left on (ADR-0019 decision 4).
	certManagerNamespace = "cert-manager"
)

// Check IDs are PERMANENT — they appear in --json output and CI configs.
const (
	IDVersionSupported    = "cluster.version-supported"
	IDStorageClassDefault = "cluster.storageclass-default"
	IDIngressClassPresent = "cluster.ingressclass-present"
	IDRBACSufficient      = "cluster.rbac-sufficient"
)

// Options carries the dependencies the cluster preflight checks close over.
type Options struct {
	// Client reads the cluster as the identity that will run the install —
	// the RBAC walk answers for whoever the kubeconfig authenticates, so the
	// preflight and the install must run as the same identity to gate
	// honestly.
	Client client.Client

	// ServerVersion reports the API server's build info; wire a discovery
	// client's ServerVersion method. A func field (client-go's own ctx-less
	// signature) so tests stub it without a discovery client.
	ServerVersion func() (*version.Info, error)
}

// Checks returns the cluster preflight checks for the given options, in the
// order they report. None carries Requires: each probes the API server
// independently, and an unreachable cluster surfaces as probe errors across
// the board rather than one failure hiding the rest (the doctor convention).
func Checks(opt Options) []check.Check {
	return []check.Check{
		versionSupportedCheck(opt),
		storageClassDefaultCheck(opt),
		ingressClassPresentCheck(opt),
		rbacSufficientCheck(opt),
	}
}

// Register adds every cluster preflight check to reg.
func Register(reg *checks.Registry, opt Options) error {
	for _, c := range Checks(opt) {
		if err := reg.Register(c); err != nil {
			return fmt.Errorf("register %s: %w", c.ID, err)
		}
	}
	return nil
}
