# ADR-0010: Validate with OpenAPI and CEL only — no admission webhook in v1alpha1

- Status: Accepted
- Date: 2026-06-11

## Context

CRDs validate three ways: OpenAPI schema constraints, CEL validation rules in the schema, and admission webhooks. A webhook is the most powerful and by far the most expensive to own: TLS certificates to rotate, an availability coupling (webhook down means every CR write in the cluster blocks), and one more deployment to debug at 11pm.

## Decision

v1alpha1 ships **no validating webhook and no mutating webhook**. Every single-object rule Orkano needs is expressible in-schema (k3s ships Kubernetes versions with CEL long since stable):

| Rule | Mechanism |
|---|---|
| `env[*]` has exactly one of `value` / `secretRef` | CEL on EnvVar |
| Worker apps cannot set `port` or `healthCheck` | CEL on AppSpec |
| Build strategy union: exactly the matching member is set | CEL on BuildStrategy |
| Build spec is immutable | CEL transition rule (`self == oldSelf`) |
| Domain host is immutable | CEL transition rule on the field |
| Formats, enums, ranges, list sizes | OpenAPI pattern/enum/min/max/maxItems |
| `env` keyed by name, server-side-apply friendly | `listType=map`, `listMapKey=name` |

Cross-object rules (duplicate Domain hosts, `appRef` pointing at a missing App) are eventual-consistency concerns: the operator detects them and reports via status conditions, the honest Kubernetes pattern — admission-time checks against other objects race anyway.

Defaulting policy: `+kubebuilder:default` only for user-meaningful values (`type`, `replicas`, `timeoutSeconds`); behavioral tunables (resources, probe timings, the Web port) default at operator render time so improvements never require a stored-object migration. Every CEL rule gets a negative fixture in `hack/testdata/invalid/` proving it actually rejects.

## Consequences

- Zero new runtime components; validation cannot be down.
- Some feedback moves from write-time to status conditions; acceptable, and matches how the rest of Kubernetes behaves.
- If a future rule provably exceeds CEL's cost budget or expressiveness, this ADR is superseded — that is the only trigger for a webhook.

## Alternatives considered

- **Validating webhook from day one** — cert rotation, availability coupling, a deployment to operate; pure cost while CEL suffices.
- **Mutating webhook for defaults** — `+kubebuilder:default` plus operator-side rendering covers it without one.
- **Validation in the dashboard only** — the dashboard is one client of the API, not its gatekeeper; `kubectl` users get no protection.
