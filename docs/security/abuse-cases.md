# Abuse cases

This catalogue is a set of concrete attacker stories checked against the architecture — it is append-only, and IDs are permanent once assigned. Where [threat-model.md](threat-model.md) works systematically per component, this document works end-to-end per attack: one attacker, one goal, one kill chain at a time.

## AC-01 — Malicious Dockerfile in a connected repo

**Attacker & precondition:** Anyone who can push to a connected repository (including a compromised contributor account) controls the contents of a Dockerfile that Orkano will execute.

**Kill chain**

1. Attacker pushes a commit whose Dockerfile runs hostile code at build time.
2. Webhook fires; the operator creates a `Build` CR and a rootless BuildKit Job runs the Dockerfile.
3. The build starts a cryptominer, attempts to read a ServiceAccount token or cluster secrets, or attempts a container escape from the BuildKit pod.
4. Any loot is exfiltrated over the network from inside the cluster.

**Impact if unmitigated:** Arbitrary code execution inside the cluster with cluster credentials, lateral movement into app namespaces, and free compute for mining.

**Mitigations**

- INV-02: build pods mount no ServiceAccount token, run under the `restricted` Pod Security level, and can egress only to GitHub and the registry — there are no cluster credentials to steal and nowhere else to send data.
- Hard CPU/memory/time limits time-box mining to one bounded Job.
- Rootless BuildKit, never a Docker socket (no daemon to pivot through).

**Verdict:** Partially mitigated — the design is sound, but rootless BuildKit under restricted PSA is unproven until the M0.5 spike lands (Risk #2 in PLANNING.md); fallbacks (tainted build node pool, gVisor/Kata) are documented but unbuilt.

**Detection:** The doctor's NetworkPolicy probe verifies the build-namespace egress deny actually blocks traffic; repeated `Build` CRs killed at the CPU or time ceiling surface in deploy history and the audit log.

## AC-02 — Stolen admin session cookie

**Attacker & precondition:** Attacker holds a valid admin session cookie, stolen off-platform (malware or physical access on the admin's machine).

**Kill chain**

1. Attacker replays the cookie against the dashboard API from their own machine.
2. They browse app configuration and deploy history as the admin.
3. They deploy a malicious app by writing an `App` CR (contained by admission policy and the dashboard's narrow RBAC).
4. They attempt a destructive action — delete app, rotate secrets — and hit step-up re-authentication, which they cannot pass.

**Impact if unmitigated:** Full, persistent admin control of the platform for as long as the session would have lived.

**Mitigations**

- ADR-0003: sessions are opaque and server-side, so the admin can revoke them instantly — deliberately not stateless JWTs.
- Step-up re-auth gates destructive actions; a cookie alone is not enough.
- INV-01 bounds the blast radius: even a fully hijacked dashboard session cannot exec into pods or dump cluster secrets.
- INV-08: every privileged action lands in the append-only audit log.

**Verdict:** Partially mitigated — preventing cookie theft on the admin's endpoint is out of scope; what Orkano controls (instant revocation, step-up gates, bounded blast radius) is in place.

**Detection:** Audit-log entries tied to the session — actions from an unfamiliar IP or at unusual hours are the signal; impersonated reads also land in the Kubernetes audit log under the human identity.

## AC-03 — Compromised npm dependency in the dashboard frontend

**Attacker & precondition:** Attacker publishes a malicious version of a package in the dashboard frontend's dependency tree, and it gets built into a shipped release.

**Kill chain**

1. The malicious version is pulled in via a lockfile update and bundled into the dashboard's JavaScript.
2. The payload runs in the admin's browser with a live session.
3. It silently drives the dashboard API as the admin — reading app config, writing `App` CRs.
4. It attempts to escalate to the cluster and stops at the dashboard's own RBAC: no cluster-admin, and secret values can be written but never read back.

**Impact if unmitigated:** Everything the admin can do in the UI, done silently — including deploying attacker-controlled apps.

**Mitigations**

- INV-01: the dashboard holds no cluster-admin and only write-only RBAC on secrets, so even total frontend compromise cannot read secret values or touch workloads directly.
- Lockfiles plus Renovate keep the dependency tree pinned, scanned, and current.
- A Content-Security-Policy to constrain exfiltration is planned for Phase 2.

**Verdict:** Partially mitigated — the server-side blast radius is bounded by INV-01, but until CSP ships, a malicious bundle can act freely within the admin's session.

**Detection:** CI dependency scanning (Renovate, Trivy) flags known-bad versions before release; audit-log entries for CRD writes the admin did not make are the runtime signal.

## AC-04 — Dashboard exposed to the internet by the user

**Attacker & precondition:** An internet-wide scanner (Shodan-class) finds the dashboard after the user has deliberately exposed it.

**Kill chain**

1. User bypasses the default exposure modes and publishes the dashboard endpoint.
2. A scanner indexes the panel within hours.
3. Attackers run credential stuffing and brute force against the login.
4. A weak or reused password without MFA yields full admin access.

**Impact if unmitigated:** The exposed-Coolify-panel scenario: full platform takeover by an anonymous internet attacker.

**Mitigations**

- INV-05: the dashboard ships ClusterIP-only and is unreachable from the internet unless explicitly exposed with SSO/MFA enforced.
- ADR-0004: `--expose public` refuses to proceed until SSO+MFA is configured; the wizard's default paths are `orkano proxy`, Tailscale, or an identity-aware proxy.
- ADR-0003: bootstrap admin has forced TOTP, lockout, and rate limits even if exposure happens anyway.
- The doctor's dashboard-exposed-without-SSO runtime check penalizes the hardening score.

**Verdict:** Mitigated by default — a user who dismantles the safeguards by hand has made an explicit, flagged choice, which is the accepted residual.

**Detection:** The doctor's dashboard-exposed-without-SSO check (and the resulting hardening-score drop); failed-login bursts in the audit log confirm active probing.
