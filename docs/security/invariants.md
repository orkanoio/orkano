# Security Invariants

An invariant is a "never" statement the architecture must keep true — not a guideline, not a default, but a property that holds even when a component is fully compromised. IDs are permanent: an invariant can be superseded by a new one (with an ADR recording why), but a number is never reused or renumbered, so INV-03 means the same thing in a five-year-old issue as it does today. Weakening or removing an invariant requires an ADR. Each entry below names the mechanism that enforces it and the doctor/CI check that will prove it — and per the project rule, checks probe behavior (attempt the forbidden thing, assert it fails) rather than reading configuration and trusting it. The companion threat model lives in [threat-model.md](threat-model.md).

| ID | Short name | Verified by |
|----|------------|-------------|
| INV-01 | The dashboard never holds cluster-admin | `rbac.dashboard-blast-radius` |
| INV-02 | Builds run as hostile code | `net.networkpolicy-enforced`, `build.apparmor-profile-loaded`, `build.canary-isolation` |
| INV-03 | Secret values never persist in the database | `db.secret-sentinel-roundtrip` |
| INV-04 | The webhook receiver is a doorbell | `webhook.receiver-blast-radius` |
| INV-05 | Private by default | `exposure.dashboard-not-public` |
| INV-06 | Only signed images run | `admission.unsigned-image-rejected` |
| INV-07 | No long-lived credentials | `creds.expiry-and-revocation` |
| INV-08 | Every privileged action is audited | `audit.append-only` |

## INV-01 — The dashboard never holds cluster-admin

**Statement.** The dashboard's entire Kubernetes permission set is CRUD on Orkano CRDs (`App`, `Build`, `Domain`, `Postgres`) in app namespaces, plus value-blind Secret writes (`create`+`update`, the verbs whose responses provably return nothing beyond the caller's own payload — ADR-0013), plus impersonation of exactly one fixed, `resourceNames`-pinned view-only identity for read views (ADR-0015). It can never exec into pods, read or delete Secrets, impersonate any other identity (`system:masters` in particular), or mutate workloads directly.

**Rationale.** The dashboard is the biggest attack surface, so a full compromise must be contained to "can deploy an app and read as a viewer" — which admission policy (INV-06) further constrains.

**Enforced by.** RBAC: the dashboard ServiceAccount is bound to a single namespaced Role scoped to `orkano.io` resources plus `create`/`update` on Secrets in the shared `orkano-apps` namespace (ADR-0005), and to exactly one cluster-scoped grant — `impersonate` on `users`/`groups` pinned by `resourceNames` to the fixed viewer identity (`orkano:viewer`/`orkano:viewers`, ADR-0015), which maps to the read-only `orkano-viewer` Role — and nothing else. The pin means the dashboard can name no privileged identity. Code structure: the dashboard API has no client code paths that touch Deployments, Pods, or Secret reads; read views go through a client impersonating only the fixed viewer identity.

**Verified by.** Today: `rbac_matrix_test.go`'s SubjectAccessReview walk (every grant allowed, every other combination denied — including a wrong-name probe proving the dashboard cannot impersonate any identity but the pinned viewer, `system:masters` in particular) and `TestSecretVerbValueBlindness` (the granted verbs' response bodies leak nothing). `rbac.dashboard-blast-radius` (planned) — authenticates as the dashboard's actual ServiceAccount and attempts `pods/exec`, Secret reads and patches, impersonation of a privileged identity, and cluster-scoped writes, asserting every one is forbidden; then asserts `App` CRUD in `orkano-apps` and an impersonated viewer read still succeed.

## INV-02 — Builds run as hostile code

**Statement.** Build pods never mount a ServiceAccount token, always run rootless under the `baseline` Pod Security level confined by the dedicated `orkano-buildkit` AppArmor profile (amended from `restricted` by ADR-0012 — `restricted` is unreachable for rootless BuildKit; the deviations are enumerated and compensated there), and can egress only to their source and the image registry.

**Rationale.** Builds execute arbitrary code from user repositories by design; the sandbox, not trust in the code, is the security boundary.

**Enforced by.** `automountServiceAccountToken: false` on every build Job; Pod Security Admission enforcing `baseline` on the build namespace plus the Localhost AppArmor profile (grants `userns` and `mount`, keeps the rest of the default confinement); a default-deny NetworkPolicy with a DNS/registry/443 egress allowlist (`config/netpol/`; enforcement proven live in the M0.5 spike and capability-probed both directions by the substrate smoke on every main push); hard CPU, memory, and wall-clock limits per Job.

**Verified by.** `net.networkpolicy-enforced` (shipped in `orkano doctor`, M3.2) — live capability probe of the NetworkPolicy substrate: creates a build-labeled control pod and two unlabeled deny canaries in the build namespace and asserts the control reaches the in-cluster registry while the unlabeled pods reach neither the registry nor the apiserver ClusterIP — the apiserver leg isolates the default-deny egress (nothing guards the apiserver's ingress), so an ingress-only CNI or a deleted egress policy cannot false-pass behind the registry's own ingress allowlist. `build.apparmor-profile-loaded` (shipped in `internal/nodeprep`, M1.5; wired into `orkano doctor --local`, M3.2) — probes that the `orkano-buildkit` profile is loaded in enforce mode, because its absence fails silently (ADR-0012). `build.canary-isolation` (planned) remains the fuller sandbox proof — runs a canary build Job that asserts from inside the pod: no token exists under `/var/run/secrets/kubernetes.io/serviceaccount`, a connection to a non-allowlisted host actually fails, and the source host and registry stay reachable; separately submits a privileged pod spec to the build namespace and asserts admission rejects it.

## INV-03 — Secret values never persist in the database

**Statement.** Secret values never persist in Orkano's PostgreSQL database — not in tables, not in audit entries, not in deploy history. They live only in Kubernetes Secrets (encrypted at rest) or the user's external vault.

**Rationale.** A dumped metadata database — the likeliest exfiltration target — must yield zero credentials.

**Enforced by.** Code structure: the schema has no column for secret values and the hand-written `sqlc` queries cannot store one; secret writes flow from the API straight to the Kubernetes Secrets API (or arrive via External Secrets Operator) without touching Postgres.

**Verified by.** `db.secret-sentinel-roundtrip` (planned) — sets an app secret to a unique sentinel value through the API, deploys, then scans every row of every Postgres table (including audit and deploy history) for the sentinel and asserts it appears nowhere — only in the Kubernetes Secret.

## INV-04 — The webhook receiver is a doorbell

**Statement.** The webhook receiver holds no secrets beyond the HMAC key and an insert-only Postgres queue role; it has no cluster access and no GitHub access.

**Rationale.** It is the only internet-facing component, so compromising it must yield nothing but the ability to ring the doorbell — the operator re-fetches all commit data from GitHub anyway and never trusts the payload.

**Enforced by.** Deployment composition: `automountServiceAccountToken: false`, no GitHub credentials in its environment, a Postgres role granted `INSERT` only on the queue table, and a NetworkPolicy allowing egress to Postgres alone.

**Verified by.** `webhook.receiver-blast-radius` (planned) — using the receiver's actual database credentials, asserts `INSERT` into the queue succeeds while `SELECT`, `UPDATE`, and `DELETE` on every table fail; from the receiver's network identity, asserts connections to the Kubernetes API server and to the GitHub API both fail.

## INV-05 — Private by default

**Statement.** The dashboard is unreachable from the internet unless it has been explicitly exposed, and explicit public exposure refuses to proceed until SSO or MFA is enforced.

**Rationale.** Publicly exposed self-hosted panels behind home-grown auth are how this product category gets breached; Shodan is full of them.

**Enforced by.** The dashboard Service ships ClusterIP-only with no Ingress; each exposure mode (`orkano proxy`, Tailscale, identity-aware proxy, public — ADR-0004) creates its route deliberately through the wizard; the `--expose public` path is a code-level guard that hard-fails when SSO/MFA is not configured.

**Verified by.** `exposure.dashboard-not-public` (shipped in `orkano doctor`, M3.2) — asserts the dashboard Service stays ClusterIP with no externalIPs and that no Ingress or Traefik IngressRoute in orkano-system routes to it. Planned strengthening once ADR-0004's exposure modes exist: probe the dashboard from outside the cluster and assert the connection actually fails unless an exposure mode was explicitly chosen; when public, send an unauthenticated request and assert SSO intercepts it before it reaches the app — the doctor's exposed-without-SSO runtime check.

## INV-06 — Only signed images run

**Statement.** Only signed images from the project's own registry run in app namespaces.

**Rationale.** Even a compromised dashboard or operator cannot launch an attacker-supplied image, because admission — not the deployer — is the gate.

**Enforced by.** A ValidatingAdmissionPolicy (built into Kubernetes, no new dependency) on app namespaces requiring digest-pinned references to the in-cluster registry with a verified cosign signature.

**Verified by.** `admission.unsigned-image-rejected` (planned) — applies a pod in an app namespace referencing an unsigned external image and asserts admission denies it; applies a signed, digest-pinned image from the in-cluster registry and asserts it admits.

## INV-07 — No long-lived credentials

**Statement.** No component holds a long-lived credential: GitHub App installation tokens expire within one hour, ServiceAccount tokens are bound and short-lived via the TokenRequest API, and user sessions are opaque and revocable instantly. The GitHub App private key lives as a Kubernetes Secret, never in the database.

**Rationale.** Stolen credentials are inevitable; short lifetimes and instant revocation shrink a breach to a window measured in minutes.

**Enforced by.** The operator mints installation tokens per use (GitHub caps them at one hour); bound tokens via the TokenRequest API instead of static ServiceAccount Secrets; opaque session IDs resolved server-side in Postgres — deliberately not stateless JWTs (ADR-0003); the GitHub App private key mounted from a Kubernetes Secret readable only by the operator.

**Verified by.** `creds.expiry-and-revocation` (planned) — mints an installation token and asserts GitHub reports an expiry of one hour or less; revokes a live dashboard session and asserts the very next request with that cookie is rejected; replays an expired bound ServiceAccount token against the API server and asserts it is refused.

## INV-08 — Every privileged action is audited

**Statement.** Every privileged action lands in an append-only audit log, with an option to ship it off-box.

**Rationale.** After an incident the audit log is the only honest narrator, so no Orkano component may be able to rewrite it.

**Enforced by.** A Postgres audit table whose application roles hold `INSERT` only — no `UPDATE` or `DELETE` grants. Orkano's own append-only audit log is the human-identity record: every privileged action is written there under the authenticated human. Dashboard reads additionally go through Kubernetes impersonation of a fixed, pinned viewer identity (ADR-0015), so cluster audit entries attribute reads to `orkano:viewer`/`orkano:viewers` rather than the dashboard ServiceAccount (the individual human is in Orkano's log, not the K8s trail — a deliberate trade for keeping every impersonate verb `resourceNames`-pinned). Optional off-box shipping so even a database compromise cannot erase history.

**Verified by.** `audit.append-only` (planned) — performs a privileged action (deleting a throwaway app) and asserts a matching audit row appears; then attempts `UPDATE` and `DELETE` on that row with every application database role and asserts both are denied.
