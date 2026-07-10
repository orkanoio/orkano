# Orkano Helm chart

Installs [Orkano](https://github.com/orkanoio/orkano) onto an existing
Kubernetes cluster (the bring-your-own-cluster path, ADR-0019). If you manage
your own nodes and want the batteries-included install instead, use
`orkano init` — it bootstraps a hardened k3s and deploys the same manifest
set this chart carries.

> **Under construction (M4.2).** This chart currently deploys the static
> substrate only — namespaces, RBAC, NetworkPolicies, the build registry and
> its internal CA, BuildKit config, the platform Postgres, and (values-gated)
> cert-manager and the External Secrets Operator. The Orkano components
> (operator, receiver, dashboard, migration Job) and the bootstrap-secrets
> Job are not templated yet, so a `helm install` does not produce a working
> PaaS until those sub-commits land — and `platform-postgres-0` stays in
> `CreateContainerConfigError` until the bootstrap Job that seeds its
> `orkano-postgres-superuser` Secret exists.

## Before installing: run the preflight

```
orkano preflight --kubeconfig <kubeconfig>
```

This is the documented, mandatory gate. It probes what this chart assumes:
Kubernetes version window, a default StorageClass, an IngressClass, the RBAC
your identity needs, and — with short-lived canary pods — that the CNI
enforces NetworkPolicy, Pod Security Admission is active, and every
build-eligible node can run AppArmor-confined builds. Exit code 0 means
install; 1 or 2 means fix the named problem first. Skipping it changes when
you learn about a gap, not whether: the same probes resurface as
`orkano doctor` checks.

## Values

| Value | Default | Meaning |
|---|---|---|
| `certManager.install` | `true` | Install the vendored cert-manager. Set `false` when the cluster already runs cert-manager (the preflight detects this). |
| `secretsVault.install` | `false` | Install the vendored, namespace-scoped External Secrets Operator (ADR-0018, opt-in). |

The component values (images/version, ACME, repo allowlist, receiver host,
ingress class, node prep) land as the chart grows toward parity with
`orkano init`'s deploy — see TASKS.md M4.2.

## Chart layout

Everything under `crds/` and `static/` is a **verbatim copy** of the repo's
`config/` manifests — the single source of truth `orkano init` embeds. A Go
drift guard (`internal/install/chart_test.go`) fails CI when either side
changes without the other. Templates only gate and load those files; they
never edit them.

Two Helm-semantics notes: Orkano's own CRDs live in `crds/`, which Helm
installs once and **never upgrades** — CRD schema migrations are owned by
`orkano upgrade` (Phase 5), not `helm upgrade`. cert-manager's CRDs ride the
values-gated template instead (so they upgrade with the release) and carry
upstream's `helm.sh/resource-policy: keep`, so `helm uninstall` never
cascade-deletes cluster-wide Certificate objects on a shared cluster —
re-check that annotation survives any cert-manager version bump.
