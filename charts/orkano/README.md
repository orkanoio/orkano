# Orkano Helm chart

Installs [Orkano](https://github.com/orkanoio/orkano) onto an existing
Kubernetes cluster (the bring-your-own-cluster path, ADR-0019). If you manage
your own nodes and want the batteries-included install instead, use
`orkano init` — it bootstraps a hardened k3s and deploys the same manifest
set this chart carries.

> **Under construction (M4.2).** This chart deploys the static substrate —
> namespaces, RBAC, NetworkPolicies, the build registry and its internal CA,
> BuildKit config, the platform Postgres, (values-gated) cert-manager and the
> External Secrets Operator — plus the Orkano components (operator, receiver,
> dashboard, migration Job, the orkano-platform ACME issuer). The
> bootstrap-secrets Job is not templated yet, so a `helm install` does not
> produce a working PaaS until that sub-commit lands: every component (and
> `platform-postgres-0`, in `CreateContainerConfigError`) stays unready until
> the generate-once Secrets it references exist (`orkano-postgres-superuser`,
> the role DSNs, `orkano-webhook-secret`, `orkano-bootstrap-token`, and the
> empty `orkano-github-app`/`orkano-oidc` placeholders).

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
| `images.repository` | `ghcr.io/orkanoio` | Registry namespace of the first-party component images (chart-only knob — `orkano init` always deploys from ghcr.io/orkanoio; override for a mirror). |
| `images.tag` | `""` (chart `appVersion`) | Tag for the operator/receiver/dashboard images. |
| `acme.email` | `""` | Registration email for the `orkano-platform` ACME ClusterIssuer. Optional. |
| `acme.production` | `false` | `false` = Let's Encrypt staging (safe default), `true` = production certificates. |
| `receiver.host` | `""` | Public hostname for the webhook receiver's Ingress. Empty = ClusterIP-only, no Ingress (INV-05); upgrade with a host to expose it later. |
| `repoAllowlist` | `[]` | GitHub repos (`owner/name`) the receiver accepts webhooks from. Empty = deny-all. |
| `ingress.className` | `traefik` | IngressClass for the receiver Ingress and the ACME HTTP-01 solver. |
| `certManager.install` | `true` | Install the vendored cert-manager. Set `false` when the cluster already runs cert-manager (the preflight detects this). |
| `secretsVault.install` | `false` | Install the vendored, namespace-scoped External Secrets Operator (ADR-0018, opt-in). |

`values.schema.json` bounds every value that lands in a rendered YAML scalar
(the same fail-closed patterns `orkano init` enforces on its flags), so a
malformed value fails at `helm install` instead of producing a broken
manifest. A `storageClassName` value is deliberately absent: both install
paths pin PVCs to the cluster's **default** StorageClass — the
`cluster.storageclass-default` preflight check requires one. The remaining
component-values work (node prep) lands as the chart grows toward parity
with `orkano init`'s deploy — see TASKS.md M4.2.

## Chart layout

Everything under `crds/` and `static/` is a **verbatim copy** of the repo's
`config/` manifests — the single source of truth `orkano init` embeds. A Go
drift guard (`internal/install/chart_test.go`) fails CI when either side
changes without the other. Templates only gate and load those files; they
never edit them.

`templates/components/` mirrors `internal/install/templates/*.yaml.tmpl` —
the per-install component manifests both paths render. Its drift guard is a
golden-render comparison (`internal/install/chart_golden_test.go`, run by
`make verify-chart` with a sha256-pinned helm 4): for equivalent values,
`helm template` must render byte-identical documents to the Go path. Keep
that directory strictly the mirror set; chart-only extras (the bootstrap
Job, node prep) live outside it.

Two Helm-semantics notes: Orkano's own CRDs live in `crds/`, which Helm
installs once and **never upgrades** — CRD schema migrations are owned by
`orkano upgrade` (Phase 5), not `helm upgrade`. cert-manager's CRDs ride the
values-gated template instead (so they upgrade with the release) and carry
upstream's `helm.sh/resource-policy: keep`, so `helm uninstall` never
cascade-deletes cluster-wide Certificate objects on a shared cluster —
re-check that annotation survives any cert-manager version bump.
