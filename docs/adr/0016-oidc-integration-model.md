# ADR-0016: OIDC sign-in via env-configured, allowlist-gated, just-in-time identities

- Status: Accepted
- Date: 2026-06-29

## Context

ADR-0003 set the bootstrap path — a one-time install token redeemed into a single local admin with forced TOTP and opaque server-side sessions — and named OIDC against a real IdP as the recommended upgrade, with the local admin retained afterward as a break-glass account. M2.5 builds that OIDC connect flow.

The PRD constrains the shape hard. "Orkano is not an identity provider" and "No user directory beyond the bootstrap admin; real identity is delegated to OIDC" are explicit non-goals. v1 is single-tenant: "one admin plus optional OIDC sign-in," with no role tiers (team/org RBAC is a v2 theme). So OIDC sign-in must add a way to authenticate against an external IdP without turning the dashboard into a directory or inventing roles it then has to manage.

Three forks had to be resolved before any code, because each changes the public surface (endpoints, the env/Secret contract, an additive `users` migration) and the security posture:

1. **How OIDC is configured** while there is no dashboard UI yet (the wizard is M2.6).
2. **Which IdP identities may sign in**, and how they map onto a single-tenant model.
3. **Whether Orkano enforces its own second factor** for OIDC users or delegates MFA to the IdP.

Migration 00003 already anticipated this: "the table generalizes to OIDC-linked identities later — that migration adds the columns it needs."

## Decision

1. **Static configuration via env + an optional Kubernetes Secret.** The dashboard reads OIDC settings from environment variables: `ORKANO_OIDC_ISSUER`, `ORKANO_OIDC_CLIENT_ID`, `ORKANO_OIDC_CLIENT_SECRET`, `ORKANO_OIDC_REDIRECT_URL`, `ORKANO_OIDC_ALLOWED_EMAILS`, `ORKANO_OIDC_ALLOWED_GROUPS`. The Deployment sources them from an optional Secret `orkano-oidc` (`envFrom … optional: true`), so a fresh install runs with OIDC off and the M2.6 wizard enables it by writing that Secret and rolling the pod — no `orkano init` OIDC flags. The **client secret lives only in the K8s Secret, never in Postgres**: it is the dashboard's own OAuth credential (like the GitHub App PEM and the enc-key), and a DB dump must not yield it. Changing OIDC config requires a Secret edit + restart; runtime reconfiguration without restart is out of scope.

2. **OIDC is enabled only when fully and safely configured (fail-closed).** OIDC turns on only when the issuer, client id, client secret, redirect URL **and at least one allowlist entry** (an email or a group) are all present. Any missing piece — including an empty allowlist — leaves OIDC disabled, so a misconfiguration can never silently expose the control plane to an entire IdP directory.

3. **The redirect URL is explicit, never derived from the request.** `ORKANO_OIDC_REDIRECT_URL` is configured outright and used verbatim. The dashboard is usually reached through a proxy (ADR-0004: `orkano proxy` / Tailscale / IAP), and the `Host` header is attacker-controllable, so a derived redirect would be both wrong and an open-redirect vector.

4. **Allowlist-gated, just-in-time provisioning.** A verified OIDC authentication is admitted only if its claims match the configured allowlist: an `email` in `ORKANO_OIDC_ALLOWED_EMAILS` (compared case-insensitively, and only when `email_verified` is true) and/or a group in `ORKANO_OIDC_ALLOWED_GROUPS`. A matched identity is provisioned just-in-time as a full-access dashboard user (single-tenant v1 has no role tiers, so every admitted human is an admin). A non-matching identity is refused after authentication, audited (INV-08), and given no session.

5. **JIT users are credential-less session anchors, not a user directory.** A provisioned OIDC user row carries no password hash and no TOTP seed (`password_hash = ''`, `totp_secret = ''`, `totp_confirmed_at` NULL) and records `(oidc_issuer, oidc_subject)`; an additive migration adds those two columns plus a unique index on the pair, which is the real identity key (the JIT lookup-or-create is keyed on it, so re-login is idempotent). The row's `username` holds the IdP email purely for display and audit readability — a username collision is a 23505 the handler reports, never a silent merge. The empty `password_hash`/`totp_secret` are a deliberate, documented exception to migration 00003's "always a bcrypt hash + TOTP seed" invariant, so no future `CHECK (char_length(password_hash) >= 60)` may be added without excluding OIDC rows. The row exists solely to anchor opaque sessions (the `sessions.user_id` foreign key), attribute audit entries to a stable id, and recognize the same human on re-login. The credential and its MFA stay at the IdP. This honors "not an identity provider": the database holds a pointer (the subject), never an OIDC credential — the same posture INV-03 takes toward app-secret values.

6. **MFA is delegated to the IdP.** OIDC users have no Orkano second factor; the IdP enforces its own MFA and account policy. The local admin keeps forced TOTP (ADR-0003) as the break-glass account. `resolveSession` admits a user that is **either** a TOTP-confirmed local admin **or** OIDC-linked, where "OIDC-linked" is the **positive** signal `oidc_subject` is non-empty — never merely `totp_confirmed_at` NULL, which an abandoned local-admin enrollment also has. Because an OIDC row is unconfirmed, the `users_single_confirmed_admin` partial unique index is untouched — the local admin stays unique and OIDC users are unbounded. The password-login path treats an OIDC username like an **unknown** user: it runs the same constant-time dummy-bcrypt comparison the no-such-user branch uses and returns the generic failure, so it neither logs the row in (an empty hash could never verify regardless) nor leaks, through response timing, that the username is an OIDC identity.

7. **The same opaque sessions; step-up is an OIDC re-auth.** An OIDC login mints the identical server-side session of ADR-0003 (instant revocation preserved). The local admin's password+TOTP step-up endpoint refuses an OIDC session outright — that identity has neither factor — so step-up for an OIDC session is instead a fresh OIDC authorization request with `prompt=login`; its callback marks the session's `reauth_at` **only after** the re-verified `sub` equals the session user's stored `oidc_subject` (the expected user id is carried in the sealed flow cookie), so the round-trip cannot mark another human's session.

8. **Authorization-code flow with state + nonce + PKCE, and a Lax flow cookie.** The connect flow is the OIDC authorization-code flow. The `state`, `nonce`, and PKCE verifier are sealed (AEAD, the existing `Cipher`) into a short-lived cookie. That flow cookie is `SameSite=Lax`, deliberately unlike the `Strict` session and challenge cookies, because the IdP→callback hop is a cross-site top-level GET that a `Strict` cookie would drop. The ID token's signature (IdP JWKS), issuer, audience (= client id), nonce, and expiry are all verified before any claim is trusted.

9. **Read views still impersonate the fixed viewer identity (ADR-0015).** An OIDC human reads through the pinned `orkano:viewer`/`orkano:viewers` identity and writes value-blind secrets through the dashboard ServiceAccount, exactly as the local admin does. The individual human is attributed in Orkano's own append-only audit log (INV-08), not the K8s audit trail. Per-human K8s attribution stays out of scope, the condition ADR-0015 deferred it to (OIDC plus team RBAC) being only half met here — single-tenant v1 still has no role tiers.

## Consequences

- **Air-gapped and no-IdP installs are unaffected.** With no issuer/allowlist configured, OIDC stays off and the bootstrap-admin path is the only way in — the self-hosted-first, air-gap requirement holds.
- **INV-01 is unchanged.** OIDC users gain no new cluster reach: reads run as the pinned viewer, writes as the narrow dashboard SA. A compromised dashboard with OIDC enabled can do exactly what it could without it.
- **No OIDC credential in the database.** A DB dump yields a subject string (a stable, non-secret identifier) but no client secret and no replayable token. The threat-model DB-dump row already covers auth material; the OIDC subject is a pointer, not a credential.
- **Exposure remains ADR-0004's concern.** The dashboard must be reachable at the configured redirect URL by the user's browser; how it is exposed (and the SSO/MFA gate on `--expose public`) is unchanged. The redirect URL being explicit is what makes a proxied deployment work.
- **The single local admin stays the recovery path.** If the IdP is down or misconfigured, the forced-TOTP local admin still logs in — the deliberate break-glass of ADR-0003.
- **Email allowlisting requires the IdP to assert `email_verified`.** An ID token with the email claim but no `email_verified: true` is treated as unverified and denied — secure, but a known incompatibility with IdPs that omit the claim (notably GitHub's OIDC). The documented mitigation is `ORKANO_OIDC_ALLOWED_GROUPS`, which keys on a group claim and needs no email verification at all.

## Alternatives considered

- **DB-backed runtime OIDC config (client secret in Postgres).** Lets the admin reconfigure without a restart, but puts an OAuth client secret in the database (a dump then yields it) and adds a config-write surface, all for a setting that changes rarely. Rejected for the boring, secure-by-default env+Secret path; the wizard automates the Secret edit.
- **Open provisioning — any authenticated IdP user becomes an admin.** Simplest, but hands full control-plane admin to the entire IdP directory; for software whose category is defined by exposed panels, that is the wrong default. Rejected for the fail-closed allowlist.
- **Link OIDC only to the pre-existing single admin.** Airtight but clunky, and it does not serve the "small team behind their IdP" use case the allowlist enables without inventing role tiers. Rejected.
- **Require an Orkano TOTP for OIDC users too.** Defense-in-depth, but it defeats the point of delegating identity, complicates JIT, and duplicates the MFA the IdP already enforces. Rejected; the IdP is the MFA authority.
- **Stateless JWT sessions for OIDC.** Rejected for the same reason ADR-0003 rejected them for the local admin: instant revocation needs server-side session state.
