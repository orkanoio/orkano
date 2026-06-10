# RBAC matrix

The security architecture made reviewable: the exact permission set of every identity. In Phase 0 this is documentation; in Phase 1 it becomes the literal Role/RoleBinding manifests, and any diff between the two is a bug. Backing invariants: INV-01 (dashboard), INV-02 (build jobs), INV-04 (receiver).

Namespaces (ADR-0005): user apps live in `orkano-apps`, builds run in `orkano-builds`, Orkano's own components in `orkano-system`.

## Dashboard ServiceAccount

| Resource (API group) | Verbs | Scope |
|---|---|---|
| apps, domains (orkano.io) | get, list, watch, create, update, patch, delete | `orkano-apps` |
| builds (orkano.io) | get, list, watch, create, delete | `orkano-apps` — create is the manual-redeploy button; delete is cancel/cleanup |
| secrets (core) | **create, patch — no get, list, or watch** | `orkano-apps` |
| users, groups (authentication) | impersonate | cluster-scoped, see compensating control below |

The write-only secrets row is the most load-bearing line in this document. The env editor must be able to store a secret value the admin types in, but nothing about editing requires reading values back — existing keys can be listed from the App's `secretRef`s, and a changed value is a blind overwrite. With `create`+`patch` but no read verbs, a fully compromised dashboard can corrupt app secrets (visible, recoverable) but cannot exfiltrate them (silent, unrecoverable). This is what makes INV-01's "cannot dump cluster secrets" hold even for app-level secrets in the dashboard's own namespace.

Impersonation is cluster-scoped by necessity (the verb cannot be namespace-bound for users/groups). Compensating control: every identity the dashboard may impersonate is bound only to the viewer role below, so impersonated reads can never exceed read-only access in `orkano-apps` — and the human identity, not the SA, lands in the Kubernetes audit log.

## Operator ServiceAccount

| Resource (API group) | Verbs | Scope |
|---|---|---|
| apps, builds, domains + `/status`, `/finalizers` (orkano.io) | get, list, watch, create, update, patch, delete | `orkano-apps` |
| deployments (apps); services (core); ingresses (networking.k8s.io) | get, list, watch, create, update, patch, delete | `orkano-apps` |
| jobs (batch) | create, get, list, watch, delete | `orkano-builds` |
| pods, pods/log (core) | get, list, watch | `orkano-apps`, `orkano-builds` |
| secrets (core) | get, create, update | `orkano-apps` — catalog connection secrets, registry pull secrets |
| certificates (cert-manager.io) | get, list, watch | `orkano-apps` — mirrors readiness into Domain status |
| leases (coordination.k8s.io) | get, create, update | `orkano-system` — leader election |
| events (core) | create, patch | `orkano-apps`, `orkano-builds` |

The operator is the most privileged Orkano identity and still holds no cluster-admin, no exec, no access outside its three namespaces. It is also the only identity that reads the GitHub App private key (a Secret in `orkano-system`) to mint ≤1 h installation tokens (INV-07).

## Receiver ServiceAccount

No Kubernetes permissions at all: `automountServiceAccountToken: false`, no Role, no RoleBinding. Its entire credential set is the webhook HMAC key and a Postgres role with `INSERT` on the queue table only (INV-04). A NetworkPolicy restricts its egress to Postgres.

## Build job ServiceAccount

No Kubernetes permissions and no token mounted (`automountServiceAccountToken: false`), `restricted` Pod Security level on `orkano-builds`, egress allowlisted to source + registry only (INV-02). Registry push credentials are per-build, injected as a Secret-backed env/file, never a SA token.

## Human roles (used via dashboard impersonation, bindable to OIDC identities)

| Role | Resources | Verbs | Scope |
|---|---|---|---|
| orkano-admin | apps, builds, domains (orkano.io) | get, list, watch, create, update, patch, delete | `orkano-apps` |
| | pods, pods/log (core) | get, list, watch | `orkano-apps` |
| orkano-viewer | apps, builds, domains (orkano.io); pods, pods/log (core) | get, list, watch | `orkano-apps` |

Humans get no secrets verbs at all in v1 — secret writes flow through the dashboard SA's write-only path, and values are never displayed.
