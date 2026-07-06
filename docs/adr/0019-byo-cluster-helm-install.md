# ADR-0019: Install onto existing clusters via a Helm chart gated by a capability-probing preflight

- Status: Proposed
- Date: 2026-07-07

## Context

PLANNING sequences two install stages: `orkano init` (stage 1, done — we control the whole
k3s stack, one configuration to test) and bring-your-own-cluster (stage 2, Phase 4 — "a Helm
install path for existing clusters... precisely when the capability-probing preflight earns
its keep"). Everything `orkano init` quietly guarantees is now variable: the CNI may not
enforce NetworkPolicy, there may be no default StorageClass, the ingress controller is
unknown, cert-manager may already be installed, PSA may be configured cluster-wide, and —
the two hard ones — Orkano has **no SSH root on the nodes**, which stage 1 uses to load the
`orkano-buildkit` AppArmor profile (ADR-0012; build pods pin `Localhost/orkano-buildkit`
and cannot start without it) and to wire each node's containerd to trust the in-cluster
registry's internal CA and resolve its cluster-DNS name (kubelet pulls resolve neither).

The mechanism-level forces: the deployable manifest set already exists as a single source of
truth (`config/` static manifests embedded via `config.StaticManifests` + the per-install
`internal/install/templates/*.yaml.tmpl` + generate-once Secrets), and duplicating it into a
chart that drifts is the failure mode to design against. Helm cannot run our preflight
(hooks execute in-cluster with the chart's own RBAC, after the interesting failures), cannot
generate-once Secrets safely under GitOps (`lookup` returns empty under `helm template` and
ArgoCD-style render-only pipelines, silently minting fresh credentials per render), and must
not fork the k3s-only pieces (the Traefik `HelmChartConfig` redirect exists only on k3s).

## Decision

The numbered decisions are this proposal's recommendations, stated declaratively so they can
be implemented as written once accepted; the four tagged **[open fork]** are exactly the ones
the sign-off section at the end reopens — hence Proposed, not Accepted.

1. **The chart is hand-maintained in-repo (`charts/orkano/`), drift-guarded against the
   embedded set — never a fork of it.** *[open fork (b)]* Static manifests (`config/crd`, `namespaces`, `rbac`,
   `netpol`, `registry`, `buildkit`, `components`, `cert-manager`) enter the chart as
   verbatim copies; a Go test asserts byte-equality with `config/` (the
   `TestEmbeddedProfileMatchesConfig` pattern — an edit to one side fails CI until both
   move). The per-install pieces (operator, receiver, dashboard, ClusterIssuer, migration
   Job, optional receiver Ingress) are translated to Helm templates whose rendered output is
   golden-compared in CI against `internal/install`'s own `renderComponents` output for the
   same values (the buildjob golden-file pattern; `helm` in CI is version-pinned and
   sha256-verified like sqlc/golangci-lint). The k3s-only Traefik redirect
   `HelmChartConfig` is excluded from the chart entirely.

2. **The preflight is `orkano preflight --kubeconfig`, a CLI command over the existing check
   registry — the documented, CI-enforced gate, not a Helm hook.** New cluster-facing checks
   (`internal/preflight/cluster`, the check framework's install face pointed at an existing
   cluster): server version within the supported window (last three minors of the frozen
   client version table), default StorageClass present (registry PVC, platform Postgres,
   catalog), IngressClass present, RBAC sufficiency via a SelfSubjectAccessReview walk over
   the verbs the chart needs, plus the pod-creating capability probes (PRD principle 9):
   NetworkPolicy actually enforced (scratch namespace, default-deny, canary pair — the
   connection must fail), PSA admission active (a canary namespace enforcing `restricted`
   must reject a privileged pod), AppArmor-capable nodes (an AppArmor-referencing canary
   must start), and the kubelet `seccompDefault` caveat ADR-0012 explicitly assigned to this
   preflight — a nil-seccomp canary reading its own `/proc/self/status` detects a kubelet
   that would flip build pods back to `RuntimeDefault` and re-break builds. Exit codes follow doctor's contract (0/1/2); the chart's README and
   NOTES.txt name the command; the conformance matrix runs it for real. Helm does not block
   on it — a user can skip it, and the same probes resurface as doctor checks post-install,
   so skipping changes when they learn, not whether.

3. **Node prep on BYO is an opt-in privileged DaemonSet (`nodePrep.enabled`), carrying the
   same trust `orkano init` already exercises as root over SSH.** *[open fork (a)]* One
   component does both node jobs: load `orkano-buildkit` into the kernel in enforce mode
   (host `/etc/apparmor.d` + `apparmor_parser`, the `internal/nodeprep` logic re-hosted) and
   wire containerd's registry trust — a `certs.d` CA drop-in plus the FQDN→ClusterIP hosts
   entry, the mechanism the M1.6 E2E's kind harness already exercises
   (`hack/ci/e2e/run.sh`'s `wire_registry_pull`). Stage 1 reaches the same end through k3s's
   own `registries.yaml` (`internal/install/registry.go`), a translation no generic cluster
   performs — so only that file's CA-fetch and hosts-entry halves carry over; the `certs.d`
   drop-in is new productized surface. It is privileged by necessity and by
   honest equivalence: stage 1 performs identical writes as root over SSH; the DaemonSet is
   the same privilege delivered through the scheduler, converging new nodes automatically.
   Clusters that forbid privileged workloads use the documented manual node-prep path
   (the same files, admin-applied); either way the result is **probed, never assumed** —
   builds fail closed without the profile (kubelet refuses the pod), and the preflight/doctor
   checks name the missing prep instead of letting the first build discover it.

4. **Coexistence is values-gated and preflight-detected, never guessed at install time.**
   `certManager.install=true` by default (bare clusters work out of the box); the preflight
   detects an existing cert-manager (its CRDs) and instructs `certManager.install=false`,
   in which case Orkano creates only its own namespaced internal-CA issuers and the
   `orkano-platform` ClusterIssuer against the cluster's cert-manager. Ingress adapters are
   `ingress.className` → a new operator `--ingress-class` flag → Domain-rendered Ingresses
   set `ingressClassName` when the flag is non-empty (today's cluster-default behavior stays
   the default when empty). Traefik and ingress-nginx are the tested adapters; the
   HTTP→HTTPS redirect story is per-adapter documentation (ingress-nginx redirects by
   default when TLS is configured; BYO-Traefik users configure their entrypoints). Gateway
   API stays the forward-looking target, not a v1 adapter.

5. **The in-cluster registry remains the one build registry on BYO; external registries stay
   post-v1.** *[open fork (d)]* The registry's netpol-guarded, unauthenticated-inside-the-fence design (accepted
   risk #9) is unchanged, which means it must never be exposed via Ingress; kubelet pull
   trust is exactly what decision 3 provides. Supporting a user-provided external registry
   (ECR/GHCR/...) would dodge node prep but drags in push credentials on build pods,
   authenticated digest resolution, and imagePullSecrets threading — a real feature, deferred
   to the Backlog, not smuggled in here.

6. **Generate-once Secrets and migrations run as an in-cluster bootstrap Job; the bootstrap
   token is never persisted or logged in plaintext.** *[open fork (c)]* The chart ships a Job (idempotent,
   `ensureSecrets`' generate-once semantics via a small operator subcommand, RBAC-pinned to
   the orkano-system Secrets it seeds) that creates the superuser/role/enc-key/webhook
   Secrets and the empty github-app/oidc placeholders, then runs `migrate`. It skips
   anything that exists, so `helm upgrade` and GitOps re-syncs never rotate credentials.
   The ADR-0003 install token is the exception to "the Job generates everything": the Job
   seeds no usable token; the user mints it with the already-documented rotate recipe
   (commit 539a857's `printBootstrapTokenRecovery`), productized as `orkano bootstrap-token
   --kubeconfig` — generate locally, store only the sha256, print plaintext exactly once at
   the terminal. A Job printing the token to pod logs would leave it readable to anyone with
   log access until TTL cleanup; refusing that keeps ADR-0003's "printed once" literal.

7. **BYO makes no claims about the substrate it does not control, and the doctor stays
   honest about it.** k3s hardening (secrets-encryption, audit log, CIS flags, etcd
   snapshots) is the cluster owner's domain; `backup.etcd-snapshot-age` already skips when
   no k3s etcd is present, and the hardening score reflects only what Orkano deploys.
   The chart's namespaces carry the same PSA labels as stage 1; whether PSA is enforced at
   all is what the preflight's capability probe answers.

### Open forks needing user sign-off (why this ADR is Proposed)

- **(a) The privileged node-prep DaemonSet (decision 3)** — it is the first privileged
  workload Orkano would ship. The alternative is manual-node-prep-only (weaker UX, zero
  privileged surface).
- **(b) Hand-maintained chart + drift guards (decision 1)** vs generating the chart from the
  embedded set with a script (the vendor-external-secrets.sh pattern, inverted — the
  recommendation disprefers it because the per-install templates need real Helm-values
  plumbing a generator would obscure; drift guards give the same one-source guarantee with
  boring, readable artifacts).
- **(c) Pure Helm + bootstrap Job (decision 6)** vs an `orkano install --kubeconfig` wrapper
  that drives preflight → secrets → chart apply from the CLI (one command, but Orkano then
  owns a Helm-execution surface and GitOps users bypass it anyway).
- **(d) In-cluster registry + node prep (decision 5)** vs pulling external-registry support
  forward into Phase 4.

## Consequences

- Two install paths now exist and both must stay green; the drift guards (byte-equality +
  golden render) are the mechanism that keeps them one artifact set, and CI grows a
  chart-lint/template job plus the conformance matrix (M4.3) to make "supported platforms" a
  tested claim.
- Builds on BYO clusters **require AppArmor-capable nodes** (ADR-0012's confinement is
  load-bearing, not decorative): GKE's COS/Ubuntu node images qualify; EKS's default
  Amazon Linux AMIs (SELinux lineage, AppArmor off) do not — which makes GKE the natural
  managed-cloud lane for the conformance matrix and an honest documented limitation, not a
  silent failure (the preflight canary names it).
- A cluster admin who declines the DaemonSet gets a working control plane, dashboard, and
  catalog but no builds until manual node prep — fail closed, with the doctor pointing at
  exactly what is missing.
- The bootstrap-token flow gains one small CLI command but never weakens ADR-0003; the Job
  path keeps the chart GitOps-safe (no `lookup`, no template-time randomness rotating
  credentials on re-render).
- Version skew becomes a supported-window constant that must be bumped alongside the frozen
  k8s.io version table — one more deliberate-bump site, guarded by the preflight test pinning
  the window.

## Alternatives considered

- **Helm hooks running the preflight in-cluster** — executes with the chart's RBAC after
  install has begun, cannot probe as the installing user, and turns refusal into a
  half-installed release; rejected.
- **Chart as the source of truth, `config/` rendered from it** — inverts a working, tested
  embed pipeline and puts Helm on `orkano init`'s critical path; rejected.
- **`helm lookup`-based generate-once secrets** — silently breaks under `helm template` and
  ArgoCD-style render-only pipelines, exactly the BYO audience; rejected.
- **Printing the bootstrap token from the Job's logs** — persists a live credential in pod
  logs until TTL; violates the spirit of ADR-0003's print-once; rejected.
- **Always-on (non-opt-in) node-prep DaemonSet** — privileged surface should be a visible
  choice on someone else's cluster, even though stage 1 exercises the same trust; rejected.
- **Exposing the in-cluster registry via Ingress for kubelet pulls** — the registry is
  unauthenticated behind NetworkPolicy (accepted risk #9); any off-cluster exposure is a
  non-starter until token-auth lands (Backlog); rejected.
