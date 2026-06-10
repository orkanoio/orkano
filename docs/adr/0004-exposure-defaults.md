# ADR-0004: Ship the dashboard unreachable from the internet by default

- Status: Accepted
- Date: 2026-06-10

## Context

A top documented project risk: users expose admin panels publicly no matter what the docs say — Shodan is full of exposed panels of exactly this product class, and a PaaS control plane holds Git credentials and executes code from repositories by design. Secure-by-default is a product requirement, not deployment advice, and security invariant INV-05 makes it testable: the dashboard is unreachable from the internet unless explicitly exposed with SSO/MFA enforced. Defaults are the only mitigation that works on people who do not read documentation.

## Decision

The dashboard Service is **ClusterIP-only by default**. No Ingress, no LoadBalancer, no NodePort — a fresh install is reachable only from inside the cluster network.

The onboarding wizard offers exposure modes in this order:

| Mode | Requires |
|------|----------|
| `orkano proxy` (default) | Nothing — the CLI tunnels into the cluster |
| Tailscale | An existing tailnet |
| Identity-aware proxy | An existing IAP in front |
| Public | OIDC SSO configured **and** MFA enforced |

Choosing public (`--expose public`) refuses to proceed until both conditions hold. This is a hard gate, not a warning the user can click through.

The webhook receiver stays the only internet-facing component by design (INV-04): stateless, HMAC-verifying, insert-only queue role, no cluster access.

A doctor check, `net.dashboard-exposed-without-sso`, probes for accidental exposure from the outside — a capability probe, not a config read — and the hardening score penalizes a hit, so an install that drifts into exposure gets caught after day one, not just at setup.

## Consequences

- Onboarding friction is consciously accepted. "Open your browser to the node IP" is off the table; the first-run path goes through `orkano proxy`, which costs a few minutes of explaining and buys INV-05.
- `orkano proxy` becomes a hard CLI requirement: the default access path must work flawlessly on day one, so it sits on the critical path and is not a cut candidate.
- Hosted demo instances must use the public + SSO path themselves — we run through our own gate.
- Some users will fight the default and file issues asking how to bypass it. That support load is accepted; it is cheaper than being the next Shodan search result.

## Alternatives considered

- **LoadBalancer by default** — the exact failure mode Orkano exists to prevent.
- **Basic-auth gate in front** — a false sense of security; credential stuffing eats it.
- **IP allowlist as the sufficient control** — home IPs rotate and VPNs defeat it; fine as defense in depth, never as the gate.
- **Documentation-only warnings** — provably ignored in this product category.
