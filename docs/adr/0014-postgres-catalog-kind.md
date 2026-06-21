# ADR-0014: Model the service catalog as an engine-specific Postgres kind

- Status: Accepted
- Date: 2026-06-21

## Context

Examples 02 and 03 already reference a Kubernetes Secret named `api-db` with key `uri`, described in the examples README as "produced by the Phase 1 Postgres catalog service; the App only ever holds its name." That producer does not exist yet. The catalog kind is a new public API — frozen on first stored object per ADR-0011 — so its name and shape have to be decided by example before any Go types, exactly as the five App archetypes were.

Three forces pin the design. v1 ships **only** PostgreSQL; Redis and MongoDB are explicit cut-line candidates that may never ship (PLANNING cut lines). The produced connection Secret is itself frozen public API: Apps hard-code its name and keys via `secretRef`. And ADR-0005's consequences note that a future service-catalog group could be added additively as `catalog.orkano.io`, leaving open whether the catalog extends the existing `orkano.io` enumeration or starts that group now.

## Decision

- A new namespaced kind **`Postgres`** in group **`orkano.io`**, version `v1alpha1` — an engine-specific kind, not a generic `Database{engine: …}`. Plural `postgreses`, set explicitly (`+kubebuilder:resource:path=postgreses`) to avoid the ambiguous auto-derived plural for a kind that already ends in `s`; the `orkano` category (`categories=orkano`, as on App/Build/Domain); no short names (ADR-0005's no-short-names rule). Future engines, if they ship, are **sibling kinds** (`Redis`, `Mongo`), each with its own version enum, conditions, and Secret contract.
- **Spec is two fields.** Everything else (replicas/HA, backups, tuning, extra users/databases, TLS, exposure) is a v2 dial that ADR-0011 lets us add additively later:
  - `version` — the PostgreSQL major series, enum `"14"|"15"|"16"|"17"`, **default `"16"`**, **immutable** (`self == oldSelf`). The operator resolves it to a digest-pinned `postgres:<version>` image. The enum may be *loosened* additively (add `"17"`), never tightened.
  - `storageSize` — the data-directory PVC size, a typed `resource.Quantity` (mirrors `Resources.CPU/Memory` in `shared_types.go`, so the apiserver validates units), **default `"10Gi"`**, **mutable** (grow-only; PVC expansion is Kubernetes-native — the operator grows on increase and rejects a shrink onto the Ready condition).
- **Status** carries a single `Ready` summary condition (reasons `Provisioning`, `ProvisionFailed`, `Available`) plus `observedGeneration` — matching the long-lived App/Domain shape, not Build's run-to-completion phase. It also echoes `secretName` so `kubectl describe` and the dashboard surface the wiring. No `phase`, no `connectionString` (a credential in status invites a log leak).
- **The produced connection Secret is named exactly `metadata.name`** — object `api-db` produces Secret `api-db` in `orkano-apps`. No suffix, no `spec.secretName` dial: the name is what Apps already reference, so deriving it is the simplest rule with one correct value. The operator is the Secret's single writer and owns it via `ownerReference`, so deletion cascades. Frozen key set: **`uri`, `host`, `port`, `database`, `username`, `password`** — the names `username` (not `user`) and `database` (not `dbname`) follow the Kubernetes-Secret convention established by CloudNativePG, Crossplane, and the Bitnami charts, so a future App splitting the `uri` finds the expected keys; `port` is a string because every Secret value is bytes. Only `uri` is load-bearing for v1; the other five are additive-safe to ship now and save a version bump later. A rename of any key, by contrast, is *not* additive (ADR-0011), so the names are chosen to be the ones we will not want to change. INV-03 holds: no value of this Secret ever appears in any CR — only the name does.
- **Upgrade story is delete-and-recreate.** A major-version bump needs `pg_upgrade`/dump+restore, too sharp to automate in v1, which is why `version` is immutable. Apps keep working across the recreate because they reference the Secret by name (INV-03), not the running pod.

### Design by example

The wished-for YAML — the contract the CRD schema must serve, paired with example 02's App, unchanged. The concrete `docs/examples/` file lands with the CRD in the next M1.4 task (a contract file before its CRD exists would fail `validate-examples`).

```yaml
# Produces Secret "api-db" (key: uri), which examples 02/03 already reference.
# Minimal: a name is the whole story; version defaults to "16", storageSize to 10Gi.
apiVersion: orkano.io/v1alpha1
kind: Postgres
metadata:
  name: api-db
  namespace: orkano-apps
spec:
  version: "16"
---
# Example 02's App, verbatim — proves no App-side change is needed. Only the
# secretRef.name "api-db" ties it to the Postgres object above.
apiVersion: orkano.io/v1alpha1
kind: App
metadata:
  name: api
  namespace: orkano-apps
spec:
  source:
    github:
      repo: alice/api
      ref: main
  build:
    strategy: Dockerfile
  port: 3000
  replicas: 2
  env:
    - name: NODE_ENV
      value: production
    - name: DATABASE_URL
      secretRef:
        name: api-db        # == the Postgres object name == the produced Secret name
        key: uri
  healthCheck:
    path: /healthz
  resources:
    cpu: 250m
    memory: 512Mi
---
# The operator-written connection Secret (Postgres is the single writer; owned so
# deletion cascades). INV-03: no value here ever appears in a CR — only the name.
# uid is assigned at create time; shown as a placeholder so the contract is exact.
apiVersion: v1
kind: Secret
metadata:
  name: api-db
  namespace: orkano-apps
  ownerReferences:
    - apiVersion: orkano.io/v1alpha1
      kind: Postgres
      name: api-db
      uid: <uid-of-the-Postgres-object>
      controller: true
type: Opaque
stringData:
  uri: postgresql://api_db:<generated>@api-db.orkano-apps.svc.cluster.local:5432/api_db
  host: api-db.orkano-apps.svc.cluster.local
  port: "5432"
  database: api_db
  username: api_db
  password: <generated>
```

## Consequences

- The reconciler (next task) renders a digest-pinned Postgres StatefulSet + a Service + the connection Secret in `orkano-apps`, gating `Ready=True` on the Secret being written so an App never wires a half-provisioned database. It needs no new RBAC: the operator's `orkano-apps` Role already carries `secrets get/create/update`, which the RBAC matrix earmarks "catalog connection secrets" — the catalog is the grant's first consumer. Cascading delete rides ownerReference GC (the apiserver's garbage collector), so the operator needs no `secrets delete`.

  > **Correction (reconciler landed 2026-06-21).** Two claims above were incomplete and are corrected by the as-built code (see `operator/internal/controller/postgres_controller.go`): (1) "needs no new RBAC" was true only of the *Secret* path — the StatefulSet and the grow-only PVC expansion required **new** grants in the operator's `orkano-apps` Role: `statefulsets` (apps group, full CRUD) and `persistentvolumeclaims` (`get`, `update`). Both landed in `config/rbac/operator.yaml` and `docs/security/rbac-matrix.md` together (the matrix test enforces parity). (2) `Ready=True` (reason `Available`) gates on the Secret being written **and** the database pod being ready (`readyReplicas >= 1`), not the Secret alone; the Secret write is a precondition that fails the reconcile first.
- Storage growth is mutable but enforced in the reconciler, not the schema — matching native PVC semantics, where the apiserver accepts a shrink and only the controller/CSI rejects it. A schema-level `self >= oldSelf` guard could be added but would then be frozen against loosening; leaving it reconciler-side keeps the option open and the boring path.
- `spec.storageClass` is deliberately omitted (a v2 dial, addable additively): the reconciler must leave `storageClassName` as `nil` — not `""`, which means "no class" — so the PVC honours the cluster default. Likewise replicas/HA, backups, tuning, extra users/databases, TLS, and exposure are all v2 dials ADR-0011 lets us add later without disturbing stored objects.
- `kubectl get orkano` keeps listing the whole platform (category `orkano`); the `catalog.orkano.io` group stays available for a genuine v2 catalog with several kinds and its own reconciler/RBAC posture.
- The frozen Secret key set is a forward bet: only `uri` is exercised by the examples, so keeping `host`/`port`/`database`/`username`/`password` now spends nothing and saves a version bump if an App ever needs the components. Removing or renaming any key later forces a version bump.
- Engine-specific means a future Redis is a *new kind*, not an enum value — slightly more code per engine, but each carries an honest Secret contract instead of a lowest-common-denominator one, and `Postgres` never has to pretend a cache is a database.
- The immutable `version` enum is sticky: because dropping a value is a tightening ADR-0011 forbids, the operator must keep shipping a digest-pinned image for **every** enum value while v1alpha1 is served — including a major that reaches end-of-life. That EOL pressure is the natural forcing function for the first version bump (v1alpha2), which is exactly when ADR-0011's conversion webhook arrives; until then, the enum only ever grows.
- Accepted sharp edges, all to be surfaced by the reconciler (validation belongs there, not a webhook — ADR-0010): a major-version change destroys the PVC and data unless the user runs their own dump/restore (backups are a deliberate v2 cut); a too-small `storageSize` (e.g. `50Mi`) that Postgres can't start on surfaces as `ProvisionFailed`; an object name that sanitizes to an invalid SQL identifier (`1-app` → `1_app`) must be caught when deriving `database`/`username`; and a user-applied Secret colliding with the produced name is prevented by the operator owning and overwriting it.
- Single-tenant v1 puts the database and its superuser-grade connection Secret in the shared `orkano-apps` namespace, so any App that can read the Secret reaches the DB — accepted risk #3 (no inter-app isolation), restated here because a database raises the stakes over a stateless App. Per-app namespaces arrive with teams in v2.

## Alternatives considered

- **Generic `Database` kind with an `engine` enum** — would centralize one reconciler family, but the produced Secret can't be honestly uniform across engines (Redis has no `username`/`database`, Mongo's `uri` differs), so a frozen `Database` Secret is either a dishonest lowest-common-denominator or fractures per-engine — defeating the enum's purpose. `Database` also mislabels a future cache as a database (violates the honest-abstraction principle).
- **`PostgresInstance`** — avoids the awkward plural, but adds an `Instance` suffix the codebase's bare-noun convention (App/Build/Domain, not AppDeployment) doesn't use; `kubectl get postgres api-db` already reads cleanly.
- **Secret named `<name>-db` (object `api`) or `<name>-credentials`** — a suffix would let the object name mirror the App (`api`), but adds implicit derivation magic over the simplest rule; the bare-name contract is also already what the examples reference.
- **`spec.secretName` field** — has exactly one correct value (must match what the App references); a dial that only adds a footgun.
- **New `catalog.orkano.io` group now** — ADR-0005 notes it only as a *later* additive option, not a commitment; standing it up for one kind doubles the RBAC/CRD-gen/reconciler-wiring surface for a solo maintainer and splits `kubectl get orkano`, with no benefit until a second catalog kind exists.
- **Mutable `version` with in-place upgrade** — would require the operator to automate `pg_upgrade`/dump+restore, a real 11pm failure mode; delete-and-recreate is honest and safe, and Apps survive it via the Secret name.
