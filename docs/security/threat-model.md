# Orkano Threat Model (v1)

A PaaS control plane holds Git credentials and runs code from the internet by design. This document is the honest accounting of what can go wrong and what we do about it. Security invariants INV-01 through INV-08 are defined in [invariants.md](invariants.md).

Found a hole? Email contact@orkano.io (see SECURITY.md for the disclosure process).

## Scope and method

**In scope:** the components Orkano ships — dashboard (React UI + Go API), webhook receiver, operator, build jobs, the in-cluster registry, and the Postgres metadata DB.

**Out of scope:** Kubernetes/k3s itself, the host OS, and GitHub's infrastructure. We configure k3s hard (secrets encryption, audit logging, CIS-aligned flags) and we verify GitHub's signatures, but we don't model bugs in their code. If your kernel or GitHub is compromised, Orkano can't save you.

**Method:** a lean STRIDE pass per component — only the categories where a real threat exists, no padding. Reviewed at each phase exit; updated when architecture changes. This is a living document, not a launch checkbox.

## Trust zones

The trust reading of the architecture: four zones, with threats living at the boundaries between them.

- **Z1 — Internet.** Anonymous and hostile. Exactly one Orkano component listens here: the webhook receiver (INV-04, INV-05).
- **Z2 — Private admin network.** The dashboard, reachable only via `orkano proxy`, Tailscale, or an identity-aware proxy unless explicitly exposed with SSO+MFA (ADR-0004).
- **Z3 — Cluster control plane.** The operator, the Orkano CRs, and etcd — where desired state lives and the most privileged code runs.
- **Z4 — Build namespace.** Hostile code zone. Every build executes a stranger's Dockerfile; we treat it as already compromised.

Threats live at the boundaries: Z1→Z3 (webhook events crossing into builds — bridged only by the insert-only Postgres queue), Z2→Z3 (dashboard writes crossing into cluster state — bridged only by CRD writes), Z3→Z4 (the operator launching jobs that run hostile code), and Z4→Z3 (build output flowing back as images that the cluster will run).

## Assets

What an attacker wants, roughly in order of how bad losing it would be:

- **GitHub App private key** — mints installation tokens for every connected repo. Lives only as a Kubernetes Secret (INV-07).
- **Webhook HMAC secret** — forging valid webhook deliveries.
- **Session store** — active admin sessions; opaque and server-side so they're instantly revocable.
- **Operator SA credentials** — the most privileged Orkano identity (still narrow RBAC, not cluster-admin).
- **User app secrets** — Kubernetes Secrets, encrypted at rest, never in Orkano's DB (INV-03).
- **Registry contents + signing identity** — poisoning either turns every deploy into an attack.
- **etcd contents** — all desired state and (encrypted) Secrets.
- **Audit-log integrity** — append-only; if an attacker can edit history, nothing else here is provable (INV-08).
- **Postgres metadata DB** — users, sessions, audit, deploy history, webhook queue. Deliberately never secret values.

## Actors

- **Anonymous internet attacker** — sees only the receiver (Z1); scans, floods, forges webhooks.
- **Malicious repo committer** — anyone who can push to a connected repo controls a Dockerfile we will execute.
- **Compromised admin session** — stolen cookie or hijacked browser on the admin network.
- **Compromised app workload** — a deployed app gone bad, attacking neighbors from inside the cluster.
- **Supply-chain attacker** — poisons our dependencies, or poisons our published artifacts to attack every downstream install.
- **Network MITM** — on-path between user↔dashboard, GitHub↔receiver, or components↔registry.

## STRIDE by component

### Dashboard (React UI + Go API)

| STRIDE | Threat | Mitigation | Residual risk |
|---|---|---|---|
| Spoofing | Stolen or brute-forced admin login | Forced TOTP on bootstrap admin, lockout + rate limits, OIDC recommended (ADR-0003) | Phished TOTP within its validity window |
| Tampering | Compromised dashboard mutates workloads or escalates | Writes Orkano CRDs only; no cluster-admin, ever (INV-01); reads via impersonation | Can still create `App` CRs — i.e. deploy apps — contained by signed-image admission (INV-06) |
| Repudiation | Admin denies a destructive action | Impersonation puts the human identity in the K8s audit trail; every privileged action lands in the append-only audit log (INV-08) | On-box audit log until ship-off-box is configured |
| Information disclosure | Dashboard or its DB dumps secrets | Secret values never persist in Orkano's database (INV-03); DB holds metadata only | DB compromise still leaks deploy history, user list, audit entries |
| Denial of service | Internet-facing panel attacked (the Coolify-class failure) | ClusterIP-only by default; `--expose public` refuses without SSO+MFA (INV-05, ADR-0004); doctor check flags exposure | Users who explicitly expose it — accepted risk #1 |
| Elevation | Hijacked session performs destructive actions | Opaque server-side sessions revocable instantly; step-up re-auth for delete/rotate (ADR-0003, INV-07) | Non-destructive actions possible until the session is revoked |

### Webhook receiver

| STRIDE | Threat | Mitigation | Residual risk |
|---|---|---|---|
| Spoofing | Forged webhook triggers a build of attacker-chosen code | HMAC verify (`X-Hub-Signature-256`) + repo allowlist + payload distrust: the webhook is a doorbell, not data — the operator re-fetches commit data from the GitHub API (INV-04, INV-07) | Leaked HMAC secret lets an attacker ring the doorbell for allowlisted repos; build content still comes from GitHub |
| Tampering | Malicious payload contents (fake refs, injected fields) | Payload is never trusted; only used as a signal to re-fetch | None meaningful — by design |
| Information disclosure | Receiver compromise leaks credentials | Holds nothing but the HMAC key and an insert-only Postgres role; no cluster access, no GitHub access (INV-04) | HMAC key itself — see Spoofing row |
| Denial of service | Webhook flood fills the queue or starves the receiver | Stateless, cheap to scale; repo allowlist drops noise pre-queue | No dedicated rate limiting yet; Postgres queue can grow under sustained flood |
| Elevation | Receiver as a beachhead into the cluster | Insert-only DB role is the entire write surface; no SA permissions beyond it | Queue spam → operator-side fetch load (operator caps concurrent builds) |

### Operator

| STRIDE | Threat | Mitigation | Residual risk |
|---|---|---|---|
| Spoofing | Stolen GitHub App private key mints tokens for all repos | Key lives only as a Kubernetes Secret readable by the operator alone; never in the DB (INV-07) | Key theft requires operator-level or etcd-level compromise; rotation is manual in v1 |
| Tampering | Poisoned queue event steers a build | Events are pointers, not payloads; operator re-fetches commit data from GitHub with a ≤1 h installation token (INV-07) | Attacker who can write the queue *and* push to an allowlisted repo deploys their code — same power as a committer |
| Repudiation | Operator actions untraceable | All reconcile actions and minted tokens land in the audit log (INV-08) | — |
| Denial of service | Reconcile storm or build-job flood exhausts the cluster | Hard CPU/mem/time limits on build jobs; workqueue rate limiting + backoff in controller-runtime | A flood still delays legitimate builds |
| Elevation | Operator compromise — the worst Orkano-component case | Narrow RBAC scoped to app namespaces and Orkano CRDs; leader election; no cluster-admin | Highest-value Orkano target: can deploy/modify apps and mint installation tokens until key rotation. Kept survivable, not impossible |

### Build job

| STRIDE | Threat | Mitigation | Residual risk |
|---|---|---|---|
| Spoofing | Build pushes an image impersonating another app | Per-build registry credentials; images digest-pinned and cosign-signed; admission verifies signatures (INV-06) | — |
| Tampering | Poisoned image smuggled into the rollout | Only signed images from the project registry admitted to app namespaces via ValidatingAdmissionPolicy (INV-06) | Signing happens post-build; the build itself decides image *contents* — a malicious Dockerfile produces a validly signed malicious image. That's the committer's existing power, not an escalation |
| Information disclosure | Build exfiltrates cluster credentials or secrets | No ServiceAccount token mounted, baseline PSA + AppArmor confinement (ADR-0012), egress allowlist: source + registry only (INV-02), enforcement capability-probed in the M0.5 spike | Exfil through the allowed 443 egress (e.g. to an attacker-controlled repo) — accepted risk #2 |
| Denial of service | Cryptominer or fork-bomb Dockerfile | Hard CPU/mem/time limits; ephemeral Jobs, never a long-lived daemon | Burns its quota until the time limit kills it |
| Elevation | Malicious Dockerfile escapes the build container to the node | Rootless BuildKit + baseline PSA confined by the dedicated AppArmor profile (ADR-0012) + no SA token (INV-02); no Docker socket, ever | The big one. Container escape via kernel bug remains plausible; baseline-not-restricted concession recorded in ADR-0012 (fallbacks tainted build node pool, gVisor/Kata stay as defense-in-depth options) |

## Cross-cutting: supply chain

**Our artifacts.** Orkano's installer and images are an attack vector for every downstream user, so supply-chain hygiene starts before any real user exists: cosign-signed images, syft SBOMs, and SLSA provenance from the throwaway v0.0.1 onward (M0.4), with distroless non-root read-only base images keeping the runtime surface minimal (ADR-0007). If you can't verify a signature on an Orkano artifact, don't run it.

**Our dependencies.** Boring, actively maintained dependencies only, scanned continuously: govulncheck and Trivy in CI, Renovate keeping versions current. A solo maintainer can't hand-audit every transitive dep — automation plus a deliberately small dependency tree is the honest mitigation.

## Accepted risks

Residual risks we consciously accept at this stage, so nobody has to discover them the hard way:

1. **Public dashboard exposure with SSO+MFA.** A user may override the private-by-default exposure (ADR-0004). We refuse without SSO+MFA and the doctor nags forever, but a publicly reachable panel is inherently more exposed than one behind `orkano proxy` or Tailscale.
2. **Egress allowlist granularity.** "GitHub + registry" is coarse — a malicious build can exfiltrate data to anywhere on GitHub (gists, attacker repos). Tighter scoping is pending the M0.5 BuildKit spike findings.
3. **Single shared `orkano-apps` namespace (ADR-0005).** No inter-app isolation in v1: a compromised app can reach its neighbors. Acceptable because v1 is single-tenant by scope — all apps belong to the same admin. Per-app namespaces are the v2 path alongside team RBAC.
4. **Build container escape.** Rootless BuildKit under baseline PSA with dedicated AppArmor confinement (ADR-0012) is strong but not a hypervisor boundary, and baseline is one level below the originally intended restricted. Accepted with the compensating controls proven in the M0.5 spike; gVisor/Kata remain backlog defense-in-depth.
5. **On-box audit log.** Until the ship-off-box option is configured, a full control-plane compromise could tamper with local audit history despite append-only semantics (INV-08).
6. **Manual GitHub App key rotation.** Installation tokens are ≤1 h (INV-07), but the App private key itself rotates manually in v1; a stolen key is valid until the admin rotates it.
7. **Metadata disclosure from Postgres.** The DB never holds secret values (INV-03), but a DB compromise still reveals who deployed what, when — metadata we accept storing because the audit trail requires it.
