// Package check defines the contract shared by the install preflight, the
// onboarding wizard, and orkano doctor: one registry of checks, three
// consumers. Checks probe capabilities — attempt the forbidden or required
// thing and observe — rather than reading configuration and trusting it.
package check

import "context"

type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	// StatusSkip means the check does not apply to this install (for
	// example, a BYO-cluster check on a bootstrapped cluster), so the
	// wizard walks past it and the hardening score ignores it.
	StatusSkip Status = "skip"
)

type Result struct {
	Status  Status `json:"status"`
	Message string `json:"message"`
}

// Check is a struct rather than an interface so that contributing a check —
// the project's designated good-first-issue surface — is one composite
// literal, with dependencies closed over by a constructor.
type Check struct {
	// ID is "area.check-name" (e.g. net.networkpolicy-enforced) and is
	// permanent once shipped: it appears in --json output and CI configs.
	ID          string
	Severity    Severity
	Summary     string
	Remediation string

	// Requires lists check IDs that must pass first; the wizard walks
	// unmet checks in this dependency order.
	Requires []string

	// Probe returning an error means the check could not run, which is
	// distinct from StatusFail: unknown must never count as hardened. A Probe
	// may run more than once per command — doctor --fix re-probes a check after
	// remediating it — so it must be safe to run again.
	Probe func(ctx context.Context) (Result, error)

	// Fix is nil when no safe automatic remediation exists; doctor --fix
	// runs it only after Probe reports StatusFail. Fix must be idempotent: it
	// may be called again on a later --fix if the check is still failing.
	Fix func(ctx context.Context) error
}
