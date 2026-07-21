package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
)

// IDCertificateExpiry is PERMANENT — it appears in --json output and CI
// configs.
const IDCertificateExpiry = "tls.certificate-expiry"

const (
	// certMinRemaining fails a certificate closer to expiry than any healthy
	// renewal ever lets it get: cert-manager renews the ACME domain leaves and
	// the registry leaf about 30 days out, so 14 days of runway means renewal
	// has been failing for weeks.
	certMinRemaining = 14 * 24 * time.Hour
	// certRenewalGrace is how long past status.renewalTime a certificate may
	// sit before renewal counts as stuck. cert-manager never advances
	// renewalTime while it retries, so this must outlast the legitimate
	// external blockers — Let's Encrypt's duplicate-certificate rate limit
	// runs a week, and Orkano's delete-and-recreate Domain semantics make
	// hitting it realistic. A week of grace still leaves every Orkano cert
	// at least three weeks of validity (the leaves renew 30 days out), and
	// this leg's real target is the long-lived certs the time-remaining
	// floor cannot see: the 10-year internal CA renews a year before expiry,
	// so its broken renewal would otherwise go unnoticed for nine years.
	certRenewalGrace = 7 * 24 * time.Hour
	// certIssuanceGrace is how long a Certificate may exist without ever
	// being issued before that counts as a failure rather than a first
	// issuance still in flight. A day, not an hour: a fresh Domain's ACME
	// order legitimately waits on DNS propagation (registrar TTLs run up to
	// 24h), and the internal-CA certs issue in seconds regardless.
	certIssuanceGrace = 24 * time.Hour
)

// certNamespaces are where Orkano's Certificates live: the internal CA and
// registry/receiver leaves in orkano-system, the per-Domain ACME leaves in
// orkano-apps. Scoped deliberately — doctor reports on Orkano's TLS, not on
// unrelated cert-manager usage elsewhere in the cluster.
var certNamespaces = []string{systemNamespace, appsNamespace}

// certificateListGVK reads cert-manager Certificates as unstructured — the
// repo-wide rule (Domain + RegistryCert reconcilers): no cert-manager Go dep.
var certificateListGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "CertificateList"}

// certificateExpiryCheck verifies every Orkano Certificate is current and its
// renewal is not stuck. cert-manager renews automatically; this check is the
// backstop for when it silently cannot (issuer unreachable, DNS moved away,
// solver misconfigured). Warning severity is a deliberate tradeoff: the
// renewal legs fire ahead of user-visible breakage, and while the expired leg
// fires after it, the critical tier (and doctor's CI exit gate) stays
// reserved for the security invariants — an availability regression shows in
// the report and drags the hardening score instead. Revisit with M3.2 item
// 5's score-threshold gate if CI needs to catch it harder.
func certificateExpiryCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDCertificateExpiry,
		Severity: check.SeverityWarning,
		Summary:  "TLS certificates are current and renewal is not stuck",
		Remediation: "inspect the named Certificate with `kubectl -n <namespace> describe certificate <name>` and the cert-manager logs; " +
			"for ACME domain certs confirm the domain's DNS still points at this cluster and ports 80/443 reach Traefik " +
			"(a brand-new domain can also just be waiting out DNS propagation or an ACME rate limit)",
		Probe: func(ctx context.Context) (check.Result, error) {
			now := opt.now()

			var certs []unstructured.Unstructured
			for _, ns := range certNamespaces {
				list := &unstructured.UnstructuredList{}
				list.SetGroupVersionKind(certificateListGVK)
				err := opt.Client.List(ctx, list, client.InNamespace(ns))
				switch {
				case meta.IsNoMatchError(err):
					// Every Orkano install deploys cert-manager (it is in the
					// static manifest set and init waits for it), so a missing
					// Certificate CRD is a broken TLS subsystem — a definitive
					// fail, never an inapplicable skip that would hide it from
					// the score.
					return check.Result{
						Status:  check.StatusFail,
						Message: "cert-manager's Certificate CRD is not installed — the install's TLS subsystem is missing; re-run `orkano init` to restore it",
					}, nil
				case err != nil:
					return check.Result{}, fmt.Errorf("list Certificates in %s: %w", ns, err)
				}
				certs = append(certs, list.Items...)
			}
			if len(certs) == 0 {
				// Init always creates the internal CA and registry
				// Certificates, so an Orkano install never legitimately has
				// zero — their absence means the PKI was deleted (or init
				// never completed), not that the check is inapplicable.
				return check.Result{
					Status: check.StatusFail,
					Message: fmt.Sprintf("no Certificates found in %s or %s — the platform PKI (internal CA, registry cert) should always exist; re-run `orkano init`",
						systemNamespace, appsNamespace),
				}, nil
			}

			var problems []string
			pending := 0
			soonestName, soonestLeft := "", time.Duration(0)
			for i := range certs {
				c := &certs[i]
				name := c.GetNamespace() + "/" + c.GetName()

				notAfter, issued, err := nestedTime(c, "status", "notAfter")
				if err != nil {
					return check.Result{}, fmt.Errorf("certificate %s: %w", name, err)
				}
				if !issued {
					if age := now.Sub(c.GetCreationTimestamp().Time); age > certIssuanceGrace {
						problems = append(problems, fmt.Sprintf("%s has never been issued (created %s ago)", name, fmtDuration(age)))
					} else {
						pending++
					}
					continue
				}

				left := notAfter.Sub(now)
				if left <= 0 {
					problems = append(problems, fmt.Sprintf("%s expired %s ago", name, fmtDuration(-left)))
					continue
				}
				if left < certMinRemaining {
					problems = append(problems, fmt.Sprintf("%s expires in %s", name, fmtDuration(left)))
					continue
				}
				renewal, hasRenewal, err := nestedTime(c, "status", "renewalTime")
				if err != nil {
					return check.Result{}, fmt.Errorf("certificate %s: %w", name, err)
				}
				if hasRenewal && now.After(renewal.Add(certRenewalGrace)) {
					problems = append(problems, fmt.Sprintf("%s renewal is overdue since %s (still valid for %s)",
						name, renewal.Format(time.RFC3339), fmtDuration(left)))
					continue
				}
				if soonestName == "" || left < soonestLeft {
					soonestName, soonestLeft = name, left
				}
			}

			if len(problems) > 0 {
				return check.Result{Status: check.StatusFail, Message: strings.Join(problems, "; ")}, nil
			}
			if soonestName == "" {
				return check.Result{
					Status:  check.StatusPass,
					Message: fmt.Sprintf("%d certificate(s) awaiting first issuance (created under %s ago)", pending, fmtDuration(certIssuanceGrace)),
				}, nil
			}
			msg := fmt.Sprintf("%d certificate(s) current; soonest expiry is %s in %s", len(certs)-pending, soonestName, fmtDuration(soonestLeft))
			if pending > 0 {
				msg += fmt.Sprintf(" (%d awaiting first issuance)", pending)
			}
			return check.Result{Status: check.StatusPass, Message: msg}, nil
		},
	}
}

// nestedTime reads an RFC3339 timestamp field from an unstructured object.
// A missing or empty field is (zero, false, nil); a non-string or unparseable
// value is an error — a timestamp we cannot read is unknown, never a pass.
func nestedTime(u *unstructured.Unstructured, fields ...string) (time.Time, bool, error) {
	s, ok, err := unstructured.NestedString(u.Object, fields...)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read %s: %w", strings.Join(fields, "."), err)
	}
	if !ok || s == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse %s %q: %w", strings.Join(fields, "."), s, err)
	}
	return t, true, nil
}

// fmtDuration renders an age or a time-to-expiry at human granularity: days
// when it is at least two days, whole hours down to one, minutes below that
// (a cert that expired minutes ago must not read "0h ago"). Negative inputs
// clamp to zero: an age computed across skewed clocks must not render
// "taken -30m ago".
func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}
