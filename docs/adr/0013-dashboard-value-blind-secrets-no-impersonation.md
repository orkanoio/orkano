# ADR-0013: Make dashboard secret writes value-blind and defer impersonation to Phase 2

- Status: Accepted
- Date: 2026-06-12

## Context

The M1.1 RBAC review found two places where the matrix's claims were weaker than its prose. First, the dashboard's `impersonate users/groups` grant was unrestricted: nothing RBAC-enforceable stopped a compromised dashboard from sending `Impersonate-Group: system:masters` and becoming cluster-admin, so INV-01 did not hold as built — the documented compensating control ("identities are bound only to orkano-viewer") constrains who *we* bind, not who the verb can name; only `resourceNames` restricts targets. Second, the "write-only" secrets grant was `create`+`patch`, but a Kubernetes PATCH response returns the mutated object, `.data` included, so the dashboard could read back any secret it knew by name — and it learns names from App `secretRef`s.

Per the probe-capabilities rule, the response bodies of every Secret mutation verb were probed against the live apiserver (v1.36, pinned by `TestSecretVerbValueBlindness`), and the results overturned intuition in both directions:

- `create`: success echoes only the caller's own payload; create on an existing name returns a bare 409 `Status`. Value-blind.
- `update` (PUT): replaces the whole object and echoes only the replacement — the old value is destroyed unread. Value-blind.
- `patch`: returns the stored object, values included, even for a patch touching only a label. Leaks.
- `delete`: returns a bare `Status` on this version — but granting it would let a compromised dashboard destroy secrets it never created (the catalog's connection secrets), and delete-then-recreate rotation leaves a window with no secret at all.

## Decision

1. The dashboard ServiceAccount holds no impersonation grant in Phase 1. Nothing consumes it before the dashboard exists, and an honest zero beats an unenforceable caveat. Phase 2 reintroduces impersonation together with its consumer, pinned via `resourceNames` to a dedicated viewer group (named alongside the OIDC work), and teaches `rbac_matrix_test.go` to express `resourceNames` in the same commit. Until then the denied SAR walk probes that `impersonate` stays dead for every identity.
2. Dashboard secret writes are `create` + `update` — exactly the value-blind pair. The env-editor UX keeps the original design intent: existing keys are listed from the App's `secretRef`s, a changed value is a blind whole-object replace. `patch` is never granted (response body leaks), `delete` is never granted (availability blast radius, non-atomic rotation), read verbs remain absent.

## Consequences

- INV-01 becomes true as stated and stronger: the dashboard cannot escalate via impersonation and cannot read back or delete any secret, including ones it wrote.
- A compromised dashboard can still blind-overwrite or name-squat any secret in `orkano-apps` — corruption, not exfiltration: visible and recoverable, the residual the original design already accepted. Catalog-owned connection secrets additionally heal on the next catalog reconcile.
- Phase 2's read views have no impersonation until it is reintroduced properly; human roles ship unbound as before and can be bound directly to OIDC identities with kubectl in the interim.
- The verb set is coupled to apiserver response-body behavior. That is deliberate: `TestSecretVerbValueBlindness` fails on any change, forcing the verb set to be reconsidered instead of silently drifting.

## Alternatives considered

- **Pin impersonation with `resourceNames` now** — builds Phase 2 API surface with no consumer and forces the OIDC group-naming decision early.
- **Create-only secrets with delete-and-recreate rotation** — the probe showed `update` is equally value-blind while keeping rotation atomic and withholding delete's blast radius; strictly better.
- **Keep `patch` and accept read-back** — documents away the matrix's most load-bearing line instead of fixing it.
- **Gate secret reads with a ValidatingAdmissionPolicy** — admission constrains requests, not response bodies; it cannot close the read-back.
