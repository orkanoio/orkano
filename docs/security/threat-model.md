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
| Spoofing | Stolen or brute-forced admin login | Forced TOTP on bootstrap admin, lockout + rate limits on the login endpoints, recovery codes for device loss (stored as one-way sha256 hashes), OIDC recommended (ADR-0003) | Phished TOTP within its validity window |
| Tampering | Compromised dashboard mutates workloads or escalates | Writes Orkano CRDs only; no cluster-admin, ever (INV-01); no impersonation grant until Phase 2 pins its targets via resourceNames (ADR-0013) | Can still create `App` CRs — i.e. deploy apps — contained by signed-image admission (INV-06) |
| Repudiation | Admin denies a destructive action | Impersonation (Phase 2, ADR-0013) puts the human identity in the K8s audit trail; every privileged action lands in the append-only audit log (INV-08) | On-box audit log until ship-off-box is configured |
| Information disclosure | Dashboard or its DB dumps secrets | User-app secret values never persist in Orkano's database (INV-03); DB holds metadata only; secret writes are value-blind create+update — no verb whose response returns stored values (ADR-0013). The dashboard's own credential store (ADR-0003 — a distinct, sanctioned category, not user-app secrets) keeps passwords as bcrypt hashes, session ids and recovery codes as one-way sha256 hashes, and the TOTP seed encrypted at rest (AES-256-GCM) under an app-layer key from a separate K8s Secret (`orkano-dashboard-enc-key`) — so a Postgres dump alone cannot bypass 2FA; the attacker also needs the enc-key Secret | A dump alone still leaks the user list, deploy history, and audit entries; combined with the enc-key Secret and a cracked bcrypt password it could yield account takeover — but the two-secret split raises the bar |
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
| Elevation | Operator compromise — the worst Orkano-component case | Narrow RBAC scoped to its three namespaces and Orkano CRDs; leader election; no cluster-admin. The orkano-system Deployment write is pinned by `resourceNames` to `orkano-registry` only (cert-rotation roll), so it cannot mutate the dashboard/receiver Deployments | Highest-value Orkano target: can deploy/modify apps, restart the registry, and mint installation tokens until key rotation. Kept survivable, not impossible |

### Build job

| STRIDE | Threat | Mitigation | Residual risk |
|---|---|---|---|
| Spoofing | Build pushes an image impersonating another app | Images are digest-pinned today; cosign signing of build outputs and the verifying ValidatingAdmissionPolicy are planned (INV-06, Phase 5 — not yet built). The registry runs without auth in v1 — a shared push credential was evaluated and rejected (every build would hold the same secret, so it stops no cross-build tag overwrite while forcing the operator to hold a registry credential; the only real fix is token-based per-build auth, deferred post-v1 — accepted risk #9). Meanwhile its ingress NetworkPolicy (`config/netpol/`) keeps every pod off the registry except build pods and the operator (whose digest-resolution manifest HEADs are gated on its pod label) — enforcement capability-probed in the substrate smoke | One build can push over another build's tag; digest pinning keeps admitted deploys unaffected (signature verification is planned, Phase 5) |
| Tampering | Poisoned image smuggled into the rollout | Only signed images from the project registry admitted to app namespaces via a ValidatingAdmissionPolicy (INV-06) — planned for Phase 5; until it ships, digest-pinning + the registry-ingress NetworkPolicy are the live controls | Signing happens post-build; the build itself decides image *contents* — a malicious Dockerfile produces a validly signed malicious image. That's the committer's existing power, not an escalation |
| Information disclosure | Build exfiltrates cluster credentials or secrets | No ServiceAccount token mounted, baseline PSA + AppArmor confinement (ADR-0012), default-deny + egress allowlist: DNS, registry, public-internet 443 only — RFC1918, link-local (cloud metadata) and CGNAT (Tailscale) excepted (`config/netpol/`, INV-02), enforcement capability-probed in the M0.5 spike and the substrate smoke | Exfil through the allowed 443 egress (e.g. to an attacker-controlled repo) — accepted risk #2 — or DNS tunneling through CoreDNS — accepted risk #8 |
| Denial of service | Cryptominer or fork-bomb Dockerfile | Hard CPU/mem/time limits; ephemeral Jobs, never a long-lived daemon | Burns its quota until the time limit kills it |
| Elevation | Malicious Dockerfile escapes the build container to the node | Rootless BuildKit + baseline PSA confined by the dedicated AppArmor profile (ADR-0012) + no SA token (INV-02); no Docker socket, ever | The big one. Container escape via kernel bug remains plausible; baseline-not-restricted concession recorded in ADR-0012 (fallbacks tainted build node pool, gVisor/Kata stay as defense-in-depth options) |

### External Secrets Operator (opt-in, ADR-0018)

Present only when the install opted in via `orkano init --secrets-vault`; a
third-party controller, so the mitigations are about bounding it, not trusting it.

| STRIDE | Threat | Mitigation | Residual risk |
|---|---|---|---|
| Elevation | Compromised ESO controller reads or rewrites cluster Secrets | The vendored render is namespace-scoped (`hack/vendor-external-secrets.sh`): the controller's RBAC is a Role in `orkano-apps` only, its cache is `--namespace`-scoped, and the only ClusterRole in the set (cert-controller) holds zero Secret access after the vendor patch — upstream's cluster-wide secrets ClusterRole never enters the cluster. Drift-guarded in `internal/install/vendored_test.go` | Full read/write of `orkano-apps` Secrets — the namespace it exists to write into; bounded by design |
| Information disclosure | A compromised dashboard drains the connected vault through ESO | Every SecretStore/ExternalSecret write is step-up gated and shape-constrained (`creationPolicy: Owner`, target-collision refusal) — ADR-0018, lands with the dashboard's ESO API in M3.1; store credentials are written value-blind (ADR-0013) and readable only by ESO | Combined with powers the dashboard already holds (App CRUD incl. `spec.command`, viewer log streaming), it can read any vault path the store credential reaches — accepted risk #10 |
| Tampering | A hostile or hijacked store endpoint feeds poisoned values into app env | TLS to the store; the store spec + credential are admin-authored under step-up (ADR-0018, lands with the dashboard's ESO API in M3.1) | Poisoned values flow into referencing apps — the same power the vault's own admin holds |

## Cross-cutting: supply chain

**Our artifacts.** Orkano's installer and images are an attack vector for every downstream user, so supply-chain hygiene starts before any real user exists: cosign-signed images, syft SBOMs, and SLSA provenance from the throwaway v0.0.1 onward (M0.4), with distroless non-root read-only base images keeping the runtime surface minimal (ADR-0007). If you can't verify a signature on an Orkano artifact, don't run it.

**Our dependencies.** Boring, actively maintained dependencies only, scanned continuously: govulncheck and Trivy in CI, Renovate keeping versions current. A solo maintainer can't hand-audit every transitive dep — automation plus a deliberately small dependency tree is the honest mitigation.

## Accepted risks

Residual risks we consciously accept at this stage, so nobody has to discover them the hard way:

1. **Public dashboard exposure with SSO+MFA.** A user may override the private-by-default exposure (ADR-0004). We refuse without SSO+MFA and the doctor nags forever, but a publicly reachable panel is inherently more exposed than one behind `orkano proxy` or Tailscale.
2. **Egress allowlist granularity.** "GitHub + registry" is coarse — a malicious build can exfiltrate data to anywhere reachable on 443 (gists, attacker repos, any HTTPS host). The M0.5 spike settled why: vanilla NetworkPolicy has no FQDN selectors, so "GitHub only" is not expressible — 443-to-public-internet (`config/netpol/orkano-builds.yaml`) is the honest approximation until an FQDN-aware egress control is consciously added (non-default CNI or egress gateway; not v1).
3. **Single shared `orkano-apps` namespace (ADR-0005).** No inter-app isolation in v1: a compromised app can reach its neighbors. Acceptable because v1 is single-tenant by scope — all apps belong to the same admin. Per-app namespaces are the v2 path alongside team RBAC.
4. **Build container escape.** Rootless BuildKit under baseline PSA with dedicated AppArmor confinement (ADR-0012) is strong but not a hypervisor boundary, and baseline is one level below the originally intended restricted. Accepted with the compensating controls proven in the M0.5 spike; gVisor/Kata remain backlog defense-in-depth.
5. **On-box audit log.** Until the ship-off-box option is configured, a full control-plane compromise could tamper with local audit history despite append-only semantics (INV-08).
6. **Manual GitHub App key rotation.** Installation tokens are ≤1 h (INV-07), but the App private key itself rotates manually in v1; a stolen key is valid until the admin rotates it.
7. **Metadata disclosure from Postgres.** The DB never holds secret values (INV-03), but a DB compromise still reveals who deployed what, when — metadata we accept storing because the audit trail requires it.
8. **DNS tunneling from build pods.** Builds need name resolution, so the egress allowlist permits queries to CoreDNS, which forwards unresolved names upstream — a classic low-bandwidth covert channel (data encoded in subdomain labels of an attacker-controlled zone). Closing it without breaking builds would need a filtering DNS proxy or FQDN-aware policies (same non-default-CNI cost as risk #2). Accepted alongside risk #2; the allowlist at least pins the channel to CoreDNS rather than any resolver on :53.
9. **In-cluster registry runs without authentication in v1.** The registry has no auth; its ingress NetworkPolicy restricts reach to build pods and the operator only (`config/netpol/`, capability-probed in the substrate smoke). A static shared push credential was evaluated and consciously rejected: every build would share one secret, so it would not stop a compromised build from overwriting another build's tag (the actual threat), while it *would* force the operator to hold a registry credential to keep digest resolution working — high cost, negligible benefit. The real fix — short-lived, repo-scoped per-build push tokens via the registry's bearer-token auth — is deferred post-v1. Until then, digest pinning keeps admitted workloads unaffected by any tag overwrite (INV-06; cosign signing of build outputs + the verifying ValidatingAdmissionPolicy are planned for Phase 5, not yet built), and `storage.delete` stays disabled so blobs can't be destroyed.
10. **Connecting an external secret store extends risk #3 to the vault slice (ADR-0018).** A fully compromised dashboard could write an ExternalSecret for any path the connected store's credential reaches, wire the produced Secret into an App it controls, and read the value from that App's logs — an amplification of the existing `orkano-apps` co-tenancy exposure, not a new boundary failure. Mitigations: a least-privilege, Orkano-scoped vault credential (the Vault recipe documents a scoped policy) and the step-up gate on every SecretStore/ExternalSecret write. Users who connect a broad credential accept that its whole slice is reachable from a dashboard compromise. The same compromise class also gains a confused-deputy read on `orkano-apps` Secrets once ESO is installed: a hand-crafted SecretStore can aim its `auth` secretRef at any Secret in the namespace (a catalog connection secret, an app's env Secret) with `server` pointing at an attacker endpoint, and ESO — which legitimately reads Secrets there — sends that credential out on the next sync. The dashboard API never accepts a caller-supplied store spec (auth is always pinned to the store's own `<store>-credentials` Secret), which closes the session-hijack/XSS class; an attacker holding the dashboard's ServiceAccount token itself bypasses the handlers and keeps the vector, bounded to `orkano-apps`. The structural close is an admission policy pinning ESO auth refs to `*-credentials` names — queued with the Phase 5 ValidatingAdmissionPolicy work.
11. **Plaintext env values are user-controlled.** An App's `spec.env[].value` legitimately carries non-secret configuration, and nothing technical stops a user from pasting a real secret into it — it then lives in the App CR (etcd) like any Kubernetes env value, outside INV-03's database promise and outside the env editor's sanctioned Secret path (whose UI copy points typed-in secrets at the secret section). Named here so the tradeoff is a documented choice, not an oversight (2026-07-06 INV-03 audit).
