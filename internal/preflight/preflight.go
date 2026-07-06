// Package preflight is the first consumer of the check registry: the set of
// capability checks orkano init runs against a node over SSH before it touches
// anything. Each check is a composite literal of the api/check contract closing
// over an Executor; ssh.reachable is the root and the rest require it, so the
// runner blocks (never silently fails) the node-probing checks when the node
// cannot be reached at all. The runner's ExitCode gates init: a critical
// failure refuses the install.
package preflight

import (
	"context"
	"slices"
	"time"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/ssh"
)

// Check IDs are permanent once shipped — they appear in --json output and CI
// configs (see api/check.Check.ID).
const (
	IDSSHReachable  = "ssh.reachable"
	IDArchSupported = "arch.supported"
	IDToolsPresent  = "tools.present"
	IDPortsFree     = "ports.free"
	IDTimeSynced    = "time.synced"
)

// DefaultRequiredPorts are the TCP ports a k3s server with embedded etcd plus
// the bundled Traefik ingress binds; any pre-existing listener on one collides
// with the install. Override per-run via Options.RequiredPorts — do not mutate
// this slice.
//
//	6443       Kubernetes API server
//	2379,2380  embedded etcd client + peer
//	10250      kubelet
//	80,443     Traefik HTTP + HTTPS ingress
var DefaultRequiredPorts = []int{6443, 2379, 2380, 10250, 80, 443}

// ExistingK3sPorts are allowed to be occupied on an idempotent rerun, but only
// when AllowExistingK3s is set and the k3s API is already answering. The ingress
// ports stay excluded: an existing listener on 80/443 can still collide with
// Traefik, so it must be fixed before init proceeds.
var ExistingK3sPorts = []int{6443, 2379, 2380, 10250}

// DefaultMaxClockSkew is the offset between the node clock and the control host
// that time.synced tolerates. Generous enough to absorb the SSH round-trip and
// the one-second granularity of `date +%s`, tight enough to catch the real
// failure mode (a node minutes or hours out, which breaks TLS and etcd).
const DefaultMaxClockSkew = 5 * time.Second

// Executor runs a command on the target node. *ssh.Client satisfies it; tests
// supply a fake. The checks close over one.
type Executor interface {
	Run(ctx context.Context, cmd string) (ssh.Result, error)
}

// Options configures the preflight check set.
type Options struct {
	// Executor reaches the node. Required.
	Executor Executor
	// Target is "user@host" for human-readable messages only.
	Target string
	// RequiredPorts overrides DefaultRequiredPorts when non-empty.
	RequiredPorts []int
	// AllowExistingK3s permits ExistingK3sPorts to be occupied when a k3s API is
	// already reachable, making `orkano init` idempotent after k3s has been
	// installed. Other occupied ports still fail.
	AllowExistingK3s bool
	// Sudo prefixes the existing-k3s readyz probe with sudo. The five preflight
	// probes deliberately run as the plain SSH user, but this one probe reads the
	// root-only k3s kubeconfig (write-kubeconfig-mode 0600), so a non-root
	// --ssh-user — which the install itself already requires passwordless sudo
	// for — would otherwise never clear the rerun allowance.
	Sudo bool
	// MaxClockSkew overrides DefaultMaxClockSkew when positive.
	MaxClockSkew time.Duration
	// Now is the control-host clock; defaults to time.Now. Injected in tests.
	Now func() time.Time
}

func (o Options) ports() []int {
	if len(o.RequiredPorts) == 0 {
		// Clone so a probe can never alias the package default.
		return slices.Clone(DefaultRequiredPorts)
	}
	return o.RequiredPorts
}

func (o Options) maxSkew() time.Duration {
	if o.MaxClockSkew <= 0 {
		return DefaultMaxClockSkew
	}
	return o.MaxClockSkew
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) target() string {
	if o.Target == "" {
		return "the node"
	}
	return o.Target
}

// Checks returns the preflight checks as composite literals closing over opt,
// in registration order (ssh.reachable first). The order the runner actually
// probes them in is the dependency order Plan computes.
func Checks(opt Options) []check.Check {
	return []check.Check{
		sshReachableCheck(opt),
		archSupportedCheck(opt),
		toolsPresentCheck(opt),
		portsFreeCheck(opt),
		timeSyncedCheck(opt),
	}
}

// Register adds the preflight checks to reg. It is the convenience orkano init
// uses to build a preflight registry; it returns the first registration error,
// which for these static checks is a programming error.
func Register(reg *checks.Registry, opt Options) error {
	for _, c := range Checks(opt) {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}
