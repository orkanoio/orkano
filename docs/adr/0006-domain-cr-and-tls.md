# ADR-0006: Model domains as a separate CR with always-on TLS

- Status: Accepted
- Date: 2026-06-11

## Context

An App needs zero or more routable hostnames, each with a certificate. Certificate issuance is asynchronous and failure-prone (Let's Encrypt rate limits, DNS misconfiguration), so per-domain state needs a home. The obvious sugar — a `domains:` list on App — creates exactly the kind of two-writer object this architecture avoids.

## Decision

- Hostnames are a separate, namespaced **`Domain`** CR: `spec.host` (immutable) and `spec.appRef.name`. There is no `domains` field on App in v1alpha1.
- **No `tls` block at all.** TLS is always on via the platform issuer configured at install time (Let's Encrypt staging by default in dev, per PLANNING risk #7); HTTP redirects to HTTPS. Zero dials.
- Reconciliation: the operator renders one Ingress rule per Domain (host → the App's Service) annotated with the platform `cluster-issuer`; cert-manager's **ingress-shim** creates the Certificate and TLS Secret. The operator watches the Certificate and mirrors readiness into `Domain.status` conditions (`Ready`, `CertificateReady`).
- `spec.host` is immutable (CEL transition rule): re-pointing a hostname is delete-and-recreate, which sidesteps cert/Ingress rename edge cases.
- Host uniqueness across Domains cannot be validated in-schema; the operator detects conflicts and sets `Ready=False, reason=HostConflict`, oldest `creationTimestamp` wins deterministically.

## Consequences

- Single writer per object: the user writes Domain spec, the operator writes Domain status — no ownership ambiguity, no sync loops.
- Per-domain failure states (rate-limited, DNS broken) live on the Domain, not in a growing blob on App.status; `App.status.url` is derived for the common case.
- Slightly more YAML for the simple case (two documents instead of one). Inline sugar on App can be added compatibly later; the reverse migration could not.
- ingress-shim over explicit Certificate objects: less code to own, cert-manager's most-traveled path.

## Alternatives considered

- **`app.spec.domains` list materializing Domain CRs** — two-way sync and ownership ambiguity; the worst kind of 11pm bug.
- **Domains inline with no Domain CR** — per-domain conditions inside App.status grow unboundedly and churn the App object on every cert renewal.
- **Explicit Certificate objects managed by the operator** — more control, more code to own; ingress-shim already does it.
- **A `tls.enabled` knob** — a dial whose only safe value is the default; omitted entirely.
