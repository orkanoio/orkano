# ADR-0018: External secrets via a vendored, opt-in, namespace-scoped External Secrets Operator

- Status: Accepted
- Date: 2026-07-06

## Context

The PRD promises "environment variables and secrets managed per app in the UI, stored only as
Kubernetes Secrets; optional external vault (Keeper, Vault, 1Password, AWS/GCP/Azure, Doppler)
via External Secrets Operator" — and INV-03 already forbids any secret value from touching
Orkano's database. M2.4's value-blind env editor covers the "typed into the UI" path; M3.1 adds
the other half: secrets that *live* in the user's external store and materialize in the cluster
as ordinary Kubernetes Secrets an App references by name.

External Secrets Operator (ESO) is the PLANNING-blessed vehicle, and it earns the "boring tech"
label with caveats worth recording: it went v1.0.0 GA in Nov 2025 after a real 2025
maintainer-burnout crisis (releases paused mid-2025, CNCF TOC health issue), and now ships
monthly 2.x releases with corporate backing; Red Hat productized the same upstream for
OpenShift 4.20+. There is no comparably broad alternative, and the crisis history is a
bus-factor fact we accept knowingly — the integration below is deliberately thin enough to
remove or swap.

Facts that pin the design (verified against ESO v2.7.0, 2026-07-06):

- ESO publishes a rendered standalone `external-secrets.yaml` per release (the official
  `helm template` output, ~1.9 MB — 98% CRD schema), so the cert-manager vendoring pattern
  (static, digest-pinned, drift-guarded) applies directly. Images are multi-arch
  (amd64+arm64), distroless, non-root, restricted-PSA-compliant, cosign-signed with SBOMs.
- The **default ClusterRole grants cluster-wide get/list/watch/create/update/delete/patch on
  every Secret in every namespace**, plus a blanket `serviceaccounts/token create`. Upstream's
  own security guide says hardening is the downstream's job. The chart's `scopedRBAC` +
  `scopedNamespace` values confine ESO to one namespace and implicitly disable the
  cluster-scoped controllers (ClusterSecretStore/ClusterExternalSecret).
- A namespaced `SecretStore` can only be referenced by `ExternalSecret`s in its own namespace;
  `ClusterSecretStore` is the cluster-wide, higher-blast-radius variant upstream recommends
  restricting.
- Provider maturity per ESO's own table: Vault / AWS / GCP / Azure are **stable,
  org-maintained**; Keeper (KSM), both 1Password paths, and Doppler are **alpha,
  community-maintained**. Every provider authenticates the store from a Kubernetes Secret
  referenced by the store spec (static-credential path), with federated no-stored-secret
  variants on the cloud providers.

## Decision

1. **Vendor ESO's rendered manifest like cert-manager — but rendered with scoped values.**
   `config/external-secrets/external-secrets.yaml` is produced once per version bump by
   `helm template` with `scopedRBAC: true`, `scopedNamespace: orkano-apps`,
   `rbac.serviceAccountTokenCreate: false`, explicit resource requests/limits, and the
   restricted-PSA namespace label — then committed, digest-pinned, and drift-guarded exactly
   like `vendored_test.go` does for cert-manager. No Helm at install or run time. The
   cluster-wide-secrets ClusterRole never enters the cluster: ESO can write Secrets in
   `orkano-apps` and nowhere else, which is the only place App-referenced Secrets live. This
   is the INV-01 posture applied to a third-party component.

2. **Opt-in, not default.** ESO is NOT part of the base install: most installs use no external
   vault, and 24 CRDs + 3 Deployments + a Secret-writing controller is surface they should not
   carry (small-surface principle; assume-breach). `orkano init --secrets-vault` adds the
   vendored set to the auto-deploy dir; because init is an idempotent converge, enabling later
   is "re-run init with the flag" — no second install mechanism, no dashboard privilege to
   deploy cluster components (which INV-01 forbids anyway). The wizard's secrets step and
   `orkano doctor` print exactly that one-liner when a store is configured but ESO is absent.
   (Opt-in over always-installed decided with the user, 2026-07-06.)

3. **No new Orkano CRD: ESO's own kinds are the honest abstraction.** The dashboard writes a
   namespaced **`SecretStore`** (not ClusterSecretStore) in `orkano-apps`, plus one
   value-blind credentials Secret (`<store>-credentials`, create+update only — the ADR-0013
   pattern) the store's `auth.secretRef` points at. Per-secret sync is an **`ExternalSecret`**
   per external key, produced by the env editor's new "from your vault" rows; the target
   Secret it materializes is referenced by the App exactly like a catalog Secret (INV-03:
   CRs hold names, never values). *As shipped (2026-07-06): syncs are created on the Vault
   page ("New sync"), not inline in the env editor — an inline picker would have duplicated
   the sync form's validation and step-up surface inside the editor, so apps wire a synced
   Secret through the env editor's existing Secret-reference rows instead; the mechanism and
   shapes are exactly as decided here.* A user can `kubectl get secretstores,externalsecrets -n
   orkano-apps` and see precisely what the UI shows — no wrapper CR to outgrow.
   Single-tenant v1 has one app namespace, so the namespaced store loses nothing a
   ClusterSecretStore would give, and drops its blast radius.
   (Namespaced SecretStore over TASKS.md's original "ClusterSecretStore connect flows"
   wording decided with the user, 2026-07-06.)

4. **Dashboard RBAC grows narrowly; every ESO write is step-up gated and shape-constrained.**
   The `orkano-dashboard` Role (orkano-apps) adds CRUD on `secretstores` + `externalsecrets`
   (external-secrets.io) — namespaced kinds, same namespace it already manages Apps in. Reads
   ride the fixed-viewer impersonation (ADR-0015); the `orkano-viewer` Role adds
   get/list/watch on the same two kinds. ALL SecretStore and ExternalSecret writes (create,
   update, delete — not just store rotation) are step-up gated: each one rewires what lands
   in app env, and they are rare operations. Dashboard-authored ExternalSecrets always set
   `target.creationPolicy: Owner` (never `Merge`/`Orphan`, which write into Secrets ESO did
   not create) and the handler refuses a `target.name` that collides with an existing Secret
   it does not manage — the same collision discipline the env editor's `reconcileEnvRefs`
   already applies — so a dashboard-driven sync cannot be aimed at a catalog connection
   Secret or another controller's Secret. The rbac-matrix doc + manifests + SAR walk move
   together, per the standing rule.

5. **Provider order: mechanism first, Vault as the blessed stable path, PRD's Keeper-first
   demoted to a documented alpha tier.** The SecretStore/ExternalSecret mechanism is
   provider-agnostic — connecting any ESO provider works day one by writing its spec. What
   v1 *documents, wizard-guides, and E2E-tests* is HashiCorp Vault first (stable+org-maintained
   upstream, self-hostable/air-gap-friendly — the audience match), then the cloud trio
   (AWS/GCP/Azure, also stable) as documented recipes. Keeper, 1Password, and Doppler are
   supported-but-alpha: documented with their upstream maturity label, UI-selectable, not
   E2E-gated. Shipping Keeper as the *flagship* path would hang Orkano's secrets story on a
   provider ESO itself labels alpha with a single community maintainer.
   (Vault-first over the PRD's "Keeper first" ordering decided with the user, 2026-07-06;
   the PRD's provider list itself is unchanged — all seven remain supported via ESO.)

6. **`secrets.store-health` (doctor) leans on ESO's own live validation, plus freshness.**
   ESO continuously validates each store's provider connectivity and stamps the result in
   `SecretStore.status` (Ready condition) and each `ExternalSecret.status`
   (Ready/SecretSynced + `refreshTime`). The doctor check fails on any store or ExternalSecret
   not Ready, or a sync older than its refresh interval allows, and verifies the target
   Secrets actually exist — reading the *result* of ESO's live probing rather than re-probing
   the vault with a second credential path. Skips (never fails) when ESO is absent, per the
   M3.2 sequencing note. A true write-then-read round-trip (PushSecret) is deliberately out:
   it requires write credentials to the user's vault, which Orkano should not ask for.

7. **Version bumps are deliberate.** ESO's rolling support (each minor EOL'd on the next
   minor's release) means Renovate does NOT auto-bump; the vendored manifest is re-rendered,
   re-verified (sha256 + image digests re-resolved as multi-arch indexes), and re-reviewed on
   an explicit bump, exactly like cert-manager. The v1beta1→v1 CRD promotion is behind us
   (v2.7.0 serves `v1` for the four core kinds), so the specs Orkano writes are stable API.

### Design by example

The wished-for YAML the UI produces — the contract to build against (a Vault store; every
other provider swaps only the `provider` block and the credentials Secret's keys):

```yaml
# The connect: one namespaced store + one value-blind credentials Secret.
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: team-vault
  namespace: orkano-apps
spec:
  provider:
    vault:
      server: https://vault.internal.example:8200
      path: secret
      version: v2
      auth:
        tokenSecretRef:
          name: team-vault-credentials
          key: token
---
apiVersion: v1
kind: Secret
metadata:
  name: team-vault-credentials   # dashboard-written, value-blind (ADR-0013)
  namespace: orkano-apps
type: Opaque
stringData:
  token: <vault token — never in a CR, never in Orkano's DB>
---
# One synced key: materializes Secret "api-stripe" the App references by name.
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: api-stripe
  namespace: orkano-apps
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: SecretStore
    name: team-vault
  target:
    name: api-stripe            # the App's secretRef.name — INV-03, name only
    creationPolicy: Owner       # never Merge/Orphan — see Decision 4
  data:
    - secretKey: STRIPE_KEY     # key inside the produced Secret
      remoteRef:
        key: apps/api/stripe    # path in the user's vault
```

## Consequences

- Air-gapped installs carry three more images (one repo, three deployments) only when they
  opt in; the manifest exercises the existing chunked-write path (1.9 MB > the cert-manager
  1 MB precedent — no new mechanism, one more consumer).
- A compromised dashboard still cannot *directly* read secret values: store credentials are
  written value-blind, ESO's controller (not the dashboard) holds the read path, and the
  dashboard's new grants are namespaced object CRUD, not secret reads. **Residual risk, named
  honestly (the ADR-0013 style):** combined with capabilities the dashboard has held since
  Phase 2 — App CRUD including `spec.command`, plus viewer-impersonated pod-log streaming — a
  fully compromised dashboard could write an ExternalSecret for any vault path the connected
  store's credential reaches, wire the produced Secret into an App it controls, and read the
  value out of that App's logs. Connecting a store therefore extends the existing
  App/Secret co-tenancy exposure (threat-model accepted risk #3) to whatever slice of the
  vault the store credential can see — the documented mitigations are a least-privilege,
  Orkano-scoped vault credential (the Vault recipe shows a scoped policy) and the step-up
  gate on every ESO write. This is an amplification of an accepted risk, not a new isolation
  boundary this ADR claims to close. A compromised ESO controller can read/write Secrets in
  `orkano-apps` only — bad, bounded; the threat-model row lands with the deploy commit
  (M3.1 item 2), like the RBAC matrix.
- The `secrets.store-health` check closes M3.2's last open item once this lands.
- Choosing namespaced SecretStore now does not foreclose ClusterSecretStore later: per-app
  namespaces (v2 teams) would flip the scoping decision, and that migration is additive
  (new store kind, same ExternalSecrets).
- The examples gain a seventh+eighth file (design-by-example above) and `validate-examples`
  learns the two ESO CRDs, mirroring how 06-postgres joined.

## Alternatives considered

- **Always-install ESO** — one less flag, but every no-vault install carries 24 CRDs, three
  controllers, and a Secret-writing component it never uses; rejected on small-surface +
  assume-breach.
- **Vendor upstream's release asset verbatim (the cert-manager mechanism, but with ESO's
  default cluster-wide RBAC intact)** — zero rendering work, but ships a controller that can
  read/write every Secret in every namespace, including orkano-system's
  superuser/enc-key/GitHub-App Secrets. cert-manager's grants are narrow by design; ESO's are
  not, and upstream explicitly delegates hardening downstream. Rejected.
- **A wrapper `SecretStoreConnection` CRD reconciled into ESO objects** — would let the
  operator own the wiring, but doubles the surface (new CRD + reconciler + conversion story)
  to hide kinds that are already clean, violating honest abstractions for no capability gain.
- **Dashboard-side vault clients (no ESO)** — no third-party controller, but re-implements
  seven providers' auth/refresh/rotation in the most privileged component, and the values
  would transit the dashboard. The entire point of ESO is that Orkano never touches them.
- **ClusterSecretStore with namespace conditions** — equivalent reach today (one namespace),
  strictly more if the conditions drift; the namespaced kind makes the boundary structural
  rather than configured. Rejected.
- **PushSecret-based write-path round-trip for doctor** — a genuine capability probe, but it
  demands write credentials to the user's vault; refusing to hold those outweighs the purity.
