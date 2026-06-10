# ADR-0005: API group orkano.io with namespaced kinds

- Status: Accepted
- Date: 2026-06-11

## Context

The CRD API group appears in every `apiVersion:` line a user ever writes and in every stored object in etcd. Renaming it after launch is a full migration for every install — effectively impossible. The GitHub org is `orkanoio`; no domain is registered yet, and Kubernetes convention is that API groups are DNS names their owner controls.

## Decision

- API group: **`orkano.io`**, version `v1alpha1`. **Hard gate:** the `orkano.io` domain must be registered before the first public tag (v0.0.1). If it proves unobtainable, the group switches to `orkano.dev` *before any CRD merges to a public repo* — cheap now, impossible later.
- Kinds: `App` (plural `apps`), `Build` (plural `builds`), `Domain` (plural `domains`). All three are **namespaced**: they map to namespaced Kubernetes objects (Deployments, Services, Ingresses, Jobs), ownerReference GC requires same-namespace owners, and namespaced scope is what keeps the dashboard's RBAC narrow (INV-01). `kubectl get apps.orkano.io` disambiguates from the built-in `apps` API group; there is no built-in *resource* named `apps`, so plain `kubectl get apps` resolves too.
- No short names — every short name is a permanent reservation in a cluster-wide namespace shared with every other CRD vendor. One category, `orkano`, on all three kinds, so `kubectl get orkano` lists everything Orkano owns.
- App namespace convention for v1: all user apps live in a single shared **`orkano-apps`** namespace, and builds run in **`orkano-builds`**. v1 is single-tenant by scope; one namespace means static RoleBindings at install time instead of operator-managed RBAC propagation. Per-app namespaces arrive with teams/RBAC in v2. The resulting lack of inter-app isolation is accepted risk #3 in the threat model.

## Consequences

- Examples, RBAC matrix, and generated CRDs all reference `orkano.io` and `orkano-apps` from day one; a domain registration (~$40/yr) becomes a release blocker, recorded in NOTICE.
- A future service-catalog group can be added additively (`catalog.orkano.io`) without touching these kinds.
- Cluster-scoped views ("all apps everywhere") stay trivial in v1 because there is exactly one namespace to look in.

## Alternatives considered

- **`core.orkano.io`** — premature subdivision; three kinds don't need a group taxonomy.
- **`orkano.orkanoio.github.io`** — permanent ugliness in every user-facing YAML to dodge a $40 risk.
- **Cluster-scoped kinds** — would force cluster-wide dashboard RBAC, violating INV-01.
- **Namespace per app in v1** — stronger isolation, but the operator would manage namespaces + RBAC propagation before the core loop works; deferred to v2 with teams.
