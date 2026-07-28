# Orkano Helm chart

Installs [Orkano](https://github.com/orkanoio/orkano) onto an existing
Kubernetes cluster (the bring-your-own-cluster path, ADR-0019). If you manage
your own nodes and want the batteries-included install instead, use
`orkano init`; it bootstraps a hardened k3s and deploys the same manifest
set this chart carries.

> **Under construction (M4.2).** The opt-in node-prep DaemonSet (AppArmor
> profile load, containerd registry CA drop-in, per-node registry
> NetworkPolicy) is not templated yet; until it lands, prepare build nodes
> manually or install with `orkano init`.

## Before installing: run the preflight

```
orkano preflight --kubeconfig <kubeconfig>
```

This is the documented, mandatory gate. It probes what this chart assumes:
Kubernetes version window, a default StorageClass, an IngressClass, the RBAC
your identity needs, and (with short-lived canary pods) that the CNI
enforces NetworkPolicy, Pod Security Admission is active, and every
build-eligible node can run AppArmor-confined builds. Exit code 0 means
install; 1 or 2 means fix the named problem first. Skipping it changes when
you learn about a gap, not whether: the same probes resurface as
`orkano doctor` checks.

## After installing: mint the bootstrap token

`helm install` seeds every generate-once platform Secret through the
`orkano-bootstrap` Job, including `orkano-bootstrap-token`. That seeded token is
deliberately unusable: the Job derives the stored sha256 from a token it
immediately discards, so the dashboard starts but nobody can log in. Mint a real
one after the install, from a machine with the cluster's kubeconfig:

```
orkano bootstrap-token --kubeconfig <kubeconfig>
```

The plaintext is printed exactly once; only its sha256 is stored. The command
also restarts `deploy/orkano-dashboard`, because the dashboard reads the hash
from its environment once at startup, and waits for that rollout: the new token
is rejected until the restarted pod is Ready. Run it again any time you need a
fresh token; the previous one stops working.

Run it after `helm install`, not before: the command writes into `orkano-system`,
which the chart creates.

The restart is an out-of-band `kubectl.kubernetes.io/restartedAt` annotation on a
Helm-managed Deployment. Helm's three-way merge drops it on the next upgrade and
an ArgoCD self-heal reverts it; each causes one extra harmless rollout and, for
GitOps users, a brief OutOfSync report. That is expected, not a bug.

## Values

| Value | Default | Meaning |
|---|---|---|
| `images.repository` | `ghcr.io/orkanoio` | Registry namespace of the first-party component images (chart-only knob: `orkano init` always deploys from ghcr.io/orkanoio; override for a mirror). |
| `images.tag` | `""` (chart `appVersion`) | Tag for the operator/receiver/dashboard images. |
| `acme.email` | `""` | Registration email for the `orkano-platform` ACME ClusterIssuer. Optional. |
| `acme.production` | `false` | `false` = Let's Encrypt staging (safe default), `true` = production certificates. |
| `receiver.host` | `""` | Public hostname for the webhook receiver's Ingress. Empty = ClusterIP-only, no Ingress (INV-05); upgrade with a host to expose it later. |
| `repoAllowlist` | `[]` | GitHub repos (`owner/name`) the receiver accepts webhooks from. Empty = deny-all. |
| `features.unsafe` | `[]` | Explicit unsafe-feature opt-ins: `source.git`, `source.zip`, and/or `build.nixpacks`. Disabled by default; the enabled set is passed to both the dashboard and operator and changing it rolls both pods. |
| `ingress.className` | `traefik` | IngressClass for the receiver Ingress and the ACME HTTP-01 solver. |
| `certManager.install` | `true` | Install the vendored cert-manager. Set `false` when the cluster already runs cert-manager (the preflight detects this). |
| `secretsVault.install` | `false` | Install the vendored, namespace-scoped External Secrets Operator (ADR-0018, opt-in). |

`values.schema.json` bounds every value that lands in a rendered YAML scalar
(the same fail-closed patterns `orkano init` enforces on its flags), so a
malformed value fails at `helm install` instead of producing a broken
manifest. Unsafe features are an enum and duplicate entries are rejected;
Helm's install notes print a prominent warning when any are enabled. A
`storageClassName` value is deliberately absent: both install
paths pin PVCs to the cluster's **default** StorageClass; the
`cluster.storageclass-default` preflight check requires one. The remaining
component-values work (node prep) lands as the chart grows toward parity
with `orkano init`'s deploy (see TASKS.md M4.2).

## Chart layout

Everything under `crds/` and `static/` is a **verbatim copy** of the repo's
`config/` manifests, the single source of truth `orkano init` embeds. A Go
drift guard (`internal/install/chart_test.go`) fails CI when either side
changes without the other. Templates only gate and load those files; they
never edit them.

`templates/components/` mirrors `internal/install/templates/*.yaml.tmpl`:
the per-install component manifests both paths render. Its drift guard is a
golden-render comparison (`internal/install/chart_golden_test.go`, run by
`make verify-chart` with a sha256-pinned helm 4): for equivalent values,
`helm template` must render byte-identical documents to the Go path. Keep
that directory strictly the mirror set; chart-only extras (the bootstrap
Job, node prep) live outside it.

The bootstrap Job and its ServiceAccount, Role and RoleBinding live in
`templates/bootstrap-job.yaml`, outside `templates/components/`, because they
are chart-only: `orkano init` seeds the same Secrets over SSH and has no
counterpart workload. Their content guard is
`internal/install/chart_test.go`'s `TestChartBootstrapJobIsChartOnly`, which
pins the Role to exactly `secrets: create`.

Two Helm-semantics notes: Orkano's own CRDs live in `crds/`, which Helm
installs once and **never upgrades**. CRD schema migrations are owned by
`orkano upgrade` (Phase 5), not `helm upgrade`. cert-manager's CRDs ride the
values-gated template instead (so they upgrade with the release) and carry
upstream's `helm.sh/resource-policy: keep`, so `helm uninstall` never
cascade-deletes cluster-wide Certificate objects on a shared cluster.
Re-check that annotation survives any cert-manager version bump.
