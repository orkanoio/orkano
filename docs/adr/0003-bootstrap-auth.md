# ADR-0003: Bootstrap auth with a one-time install token, forced TOTP, and opaque sessions

- Status: Accepted
- Date: 2026-06-10

## Context

Orkano is not an identity provider — that is an explicit PRD non-goal. Real identity belongs to a real IdP via OIDC. But there has to be a way into a fresh install before any IdP exists, and that way in must also work air-gapped.

This is exactly where our product category fails. Self-hosted PaaS panels with homegrown auth, exposed to the internet, are a Shodan staple. Whatever bootstrap mechanism we ship will be the only thing standing between the internet and a control plane that holds Git credentials and runs code by design. It has to be small, hard to misuse, and clearly a stepping stone — not a user directory in disguise.

This ADR covers the bootstrap path and session model. How the dashboard is (not) exposed to the network is ADR-0004; together they back INV-05 and INV-07.

## Decision

- `orkano init` prints a one-time install token. The first login redeems it to create the single local admin account. The token is single-use and useless afterward.
- Creating that account requires a strong password **and TOTP enrollment in the same flow** — there is no state where the admin exists without a second factor. Recovery codes are issued at enrollment.
- Passwords are stored as bcrypt hashes. Auth endpoints get account lockout and rate limits.
- Sessions are opaque random IDs stored server-side in Postgres — deliberately not stateless JWTs. An admin session must be revocable instantly, which means the server keeps the state (INV-07).
- Destructive actions (delete app, rotate secrets) require step-up re-authentication — a live session is not enough.
- OIDC against any real IdP (Keycloak or Authentik for self-hosters) is the recommended upgrade path. The local admin is retained afterward as a break-glass account, not for daily use.

## Consequences

- A `sessions` table and one Postgres lookup per request. At single-tenant scale this is noise; we trade a few milliseconds for instant revocation.
- A TOTP recovery flow (recovery codes, and a documented break-glass path if those are lost too) must exist before v1 — forced MFA without recovery is a lockout machine.
- "Install token printed but never redeemed" is a live credential lying in a terminal scrollback, so it becomes an `orkano doctor` check.
- Admins cannot opt out of TOTP. That friction is consciously accepted: we would rather lose an impatient user than ship the next exposed panel.

## Alternatives considered

- **Stateless JWTs** — instant revocation is impossible without a server-side denylist, which rebuilds the state JWTs were supposed to remove.
- **Email magic links** — mandatory SMTP on first boot violates self-hosted-first and breaks air-gapped installs.
- **Passkey-only** — the recovery UX (device loss, cross-device enrollment) is too heavy for v1; on the backlog as an additional factor.
- **No local auth / OIDC-only** — an air-gapped install with no IdP could never be bootstrapped at all.
