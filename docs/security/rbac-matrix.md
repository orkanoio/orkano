# RBAC matrix

The security architecture made reviewable: the exact permission set of every identity. In Phase 0 this is documentation; in Phase 1 it becomes the literal Role/RoleBinding manifests, and any diff between the two is a bug. Backing invariants: INV-01 (dashboard), INV-02 (build jobs), INV-04 (receiver).

Namespaces (ADR-0005): user apps live in `orkano-apps`, builds run in `orkano-builds`, Orkano's own components in `orkano-system`.

Scope: this matrix covers the identities in `config/rbac`, which is what both install paths deploy. The Helm chart additionally ships one chart-only identity with no `orkano init` counterpart: ServiceAccount `orkano-bootstrap` in `orkano-system`, holding create-only grants on Secrets and ConfigMaps, used by the one-shot Secret and repository-policy bootstrap Jobs (`charts/orkano/templates/bootstrap-job.yaml`, which carries the shared RBAC reasoning including what unpinned `create` leaves reachable).

## Dashboard ServiceAccount

| Resource (API group) | Verbs | Scope |
|---|---|---|
| apps, domains, postgreses, mongoes (orkano.io) | get, list, watch, create, update, patch, delete | `orkano-apps` |
| secretstores, externalsecrets (external-secrets.io) | get, list, watch, create, update, patch, delete | `orkano-apps`: the external-vault connect (SecretStore) and per-key sync (ExternalSecret) objects the UI writes (ADR-0018); configuration pointers, never secret values (the store credential lives in a Secret under the value-blind row below); every write is step-up gated. Granted unconditionally; RBAC is name-based, so the rule is inert until the opt-in secrets-vault install adds the ESO CRDs |
| secrets (core) | **create, update (no get, list, watch, patch, or delete)** | `orkano-apps` |
| secrets[orkano-github-app] (core) | update | `orkano-system`: the GitHub App credential the manifest flow writes for the operator to read (INV-07); value-blind update, resourceNames-pinned, no get/create/delete |
| secrets[orkano-webhook-secret] (core) | update | `orkano-system`: the webhook HMAC secret the manifest flow adopts from GitHub for the receiver to verify; same value-blind update pin |
| secrets[orkano-oidc] (core) | update | `orkano-system`: the OIDC client configuration the wizard's connect step writes for the dashboard's own next restart (ADR-0016); same value-blind update pin, write gated on step-up re-auth |
| configmaps[orkano-repo-allowlist] (core) | get, update | `orkano-system`: live exact-owner/repository policy shared by the dashboard and receiver; non-secret, resourceNames-pinned, with update gated on step-up re-auth and recorded in the append-only audit log |
| users[orkano:viewer], groups[orkano:viewers] (core) | impersonate | cluster-scoped: read views run as a fixed, fully resourceNames-pinned viewer identity (ADR-0015); no other user or group can be named |

The value-blind secrets row is the most load-bearing line in this document. The env editor must be able to store a secret value the admin types in, but nothing about editing requires reading values back. Existing keys can be listed from the App's `secretRef`s, and a changed value is a blind whole-object replace. `create` and `update` are exactly the mutation verbs whose response bodies provably return nothing beyond the caller's own payload; `patch` is excluded because a PATCH response returns the stored object, values included, even for a patch that touches only a label, and `delete` is excluded because the dashboard has no business destroying secrets it didn't create (ADR-0013; the response-body behavior is pinned against the live apiserver by `TestSecretVerbValueBlindness`). With this set, a fully compromised dashboard can corrupt app secrets (visible, recoverable) but cannot exfiltrate them (silent, unrecoverable). This is what makes INV-01's "cannot dump cluster secrets" hold even for app-level secrets in the dashboard's own namespace.

The three `orkano-system` Secret rows plus the repository-policy ConfigMap row are the dashboard's only reach outside `orkano-apps`, and every object name is pinned. The GitHub App manifest flow exchanges GitHub's manifest code for the App private key and webhook secret, then writes them where the operator (to mint installation tokens, INV-07) and receiver (to verify webhook signatures, INV-04) read them; the wizard's OIDC connect step writes the IdP client configuration (`orkano-oidc`, ADR-0016) that the dashboard's own Deployment mounts via per-key optional `secretKeyRef`s on its next restart. The Secret grant is `update` only, `resourceNames`-pinned to exactly those three Secrets: not `get` (value-blind, so a compromised dashboard can rotate but never exfiltrate the App private key, webhook secret, or OIDC client secret), not `create` (`create` cannot be `resourceNames`-pinned, so the install pre-creates placeholders), and not any other Secret (the superuser password, dashboard encryption key, and bootstrap-token hash stay unreachable). `orkano-oidc` is the one self-referential write, so its endpoint gates on step-up and the Deployment mounts explicit per-key `secretKeyRef`s rather than `envFrom`, keeping a hostile write's blast radius to the named OIDC configuration.

`orkano-repo-allowlist` is deliberately a ConfigMap rather than a database setting: the internet-facing receiver must keep enforcing policy through a database outage, while receiving no Kubernetes token or API permission (INV-04). The kubelet projects that one ConfigMap into the receiver pod, which reloads the file and fails closed when it is missing or malformed. The dashboard's direct Kubernetes reach is only `get` and `update` on that exact non-secret name: `get` supplies the `resourceVersion` required by replacement updates, stale clients receive 409, and the handler preserves all metadata and unrelated keys. The update route requires step-up re-authentication and durably appends a correlated audit intent before any Kubernetes mutation, then appends `success`, `failure`, or `indeterminate`; a detached read-back resolves transport errors whenever the live state can still be established (INV-08). No `list`, `watch`, `create`, `patch`, or `delete` is granted.

The dashboard's read views run under an impersonated viewer identity (ADR-0013/ADR-0015), so the cluster's RBAC and audit trail attribute a read to a view-only identity rather than the dashboard ServiceAccount. An unrestricted `impersonate` verb can name `system:masters` (cluster-admin) and only `resourceNames` can restrict its targets, so the grant is pinned to exactly one fixed user (`orkano:viewer`) and one fixed group (`orkano:viewers`): the dashboard can name no other identity, and the impersonated group is bound to the read-only `orkano-viewer` Role. The individual human is attributed in Orkano's own append-only audit_log (INV-08). Because `impersonate` on `users`/`groups` is cluster-scoped, this grant is a ClusterRole (`orkano-dashboard-impersonate`), not part of the namespaced dashboard Role; `rbac_matrix_test` proves the pin binds by asserting the dashboard cannot impersonate any other name (e.g. `system:masters`).

## Operator ServiceAccount

| Resource (API group) | Verbs | Scope |
|---|---|---|
| apps, builds, domains, postgreses, mongoes + `/status`, `/finalizers` (orkano.io) | get, list, watch, create, update, patch, delete | `orkano-apps` |
| deployments, statefulsets (apps); services (core); ingresses, networkpolicies (networking.k8s.io) | get, list, watch, create, update, patch, delete | `orkano-apps`. Statefulsets back the Postgres and Mongo catalog kinds; the optional Pgweb and Mongo Express workloads are internal-only behind operator-owned NetworkPolicies |
| persistentvolumeclaims (core) | get, update | `orkano-apps`: grows catalog database data volumes; volumeClaimTemplates are immutable so PVCs are patched directly, read uncached so no list/watch |
| jobs (batch) | create, get, list, watch, delete | `orkano-builds` |
| pods, pods/log (core) | get, list, watch | `orkano-apps`, `orkano-builds` |
| configmaps[orkano-registry-ca] (core) | get | `orkano-builds`: the internal CA bundle published for build pods; the Build controller verifies its registry manifest HEAD (digest resolution, INV-06) against the same trust root, read uncached so no list/watch grant exists |
| secrets (core) | get, create, update | `orkano-apps`: catalog connection secrets, registry pull secrets |
| secrets[orkano-github-app] (core) | get | `orkano-system`: the GitHub App private key, read to mint ≤1 h installation tokens (INV-07); resourceNames-pinned so the operator can read no other Secret in its own namespace |
| certificates (cert-manager.io) | get, list, watch | `orkano-apps`, `orkano-system`: mirrors readiness into Domain status; tracks the registry cert's issuance revision |
| deployments (apps) | list, watch | `orkano-system`: informer feed for the registry rotation controller; collection requests carry no object name, so resourceNames cannot constrain them |
| deployments[orkano-registry] (apps) | get, update | `orkano-system`. Rolls the registry pod when its TLS cert renews (distribution loads the keypair only at startup); mutation pinned by resourceNames to the one Deployment the controller owns, and deliberately no secrets read |
| leases (coordination.k8s.io) | get, create, update | `orkano-system`: leader election |
| events (core) | create, patch | `orkano-apps`, `orkano-builds`, `orkano-system`: last scope is the leader-election events controller-runtime emits on the Lease |

The operator is the most privileged Orkano identity and still holds no cluster-admin, no exec, no access outside its three namespaces. It is also the only identity that reads the GitHub App private key (a Secret in `orkano-system`) to mint ≤1 h installation tokens (INV-07).

## Receiver ServiceAccount

No Kubernetes permissions at all: `automountServiceAccountToken: false`, no Role, no RoleBinding. Its entire credential set is the webhook HMAC key and a Postgres role with `INSERT` on the queue table only (INV-04). A NetworkPolicy restricts its egress to Postgres.

## Build job ServiceAccount

No Kubernetes permissions and no token mounted (`automountServiceAccountToken: false`), `baseline` Pod Security level on `orkano-builds` with the dedicated AppArmor profile (ADR-0012), egress allowlisted to source + registry only (INV-02). Registry push credentials are per-build, injected as a Secret-backed env/file, never a SA token.

## Human roles (bindable to OIDC identities; consumed via the dashboard's impersonation, ADR-0015)

| Role | Resources | Verbs | Scope |
|---|---|---|---|
| orkano-admin | apps, builds, domains, postgreses, mongoes (orkano.io) | get, list, watch, create, update, patch, delete | `orkano-apps` |
| | pods, pods/log (core) | get, list, watch | `orkano-apps` |
| orkano-viewer | apps, builds, domains, postgreses, mongoes (orkano.io); pods, pods/log (core); secretstores, externalsecrets (external-secrets.io) | get, list, watch | `orkano-apps`: the dashboard's impersonation target, bound to the orkano:viewers group; the ESO kinds back the vault status views (ADR-0018) and hold configuration, never values |
| | certificates (cert-manager.io) | list | `orkano-apps`. The dashboard doctor's TLS check lists the per-Domain ACME leaf Certificates; status only, never values |
| | pods, pods/log (core) | get, list, watch | `orkano-builds`: historical and live BuildKit output for Build attempts selected in the dashboard; no Jobs or Secrets access |
| | services[orkano-dashboard] (core); deployments[orkano-operator], deployments[orkano-receiver], deployments[orkano-registry], deployments[orkano-dashboard] (apps); statefulsets[orkano-postgres] (apps) | get | `orkano-system`: the dashboard doctor's platform-components, unsafe-features and dashboard-exposure checks read these control-plane workloads and the dashboard Service by name |
| | ingresses (networking.k8s.io); ingressroutes, ingressroutetcps (traefik.io); certificates (cert-manager.io) | list | `orkano-system`. The exposure check lists Ingresses and Traefik IngressRoutes that might route to the dashboard, and the TLS check lists cert-manager Certificates |
| | nodes (core) | get, list | cluster-scoped: read-only node inventory for the dashboard's Settings page and the setup wizard's cluster step; no watch (the views poll) |
| | etcdsnapshotfiles (k3s.cattle.io) | list | cluster-scoped. The dashboard doctor's etcd-backup check reads k3s's snapshot records; list only, no watch |
| | namespaces[kube-system] (core) | get | cluster-scoped: the etcd-backup check reads the cluster's age from kube-system when no snapshots exist |

Humans get no secrets verbs at all in v1. Secret writes flow through the dashboard SA's value-blind path, and values are never displayed.
