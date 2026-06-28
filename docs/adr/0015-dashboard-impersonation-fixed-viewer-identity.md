# ADR-0015: Dashboard read views impersonate a fixed, fully pinned viewer identity

- Status: Accepted
- Date: 2026-06-29

## Context

ADR-0013 dropped the dashboard's impersonation grant entirely and committed Phase 2 to reintroduce it "together with its consumer, pinned via `resourceNames` to a dedicated viewer group (named alongside the OIDC work)." M2.4's read views are that consumer: list and get over the Orkano resources the viewer can see (`App`/`Build`/`Domain`/`Postgres`, plus pods and pod logs) run under an impersonated identity so the cluster's RBAC and audit trail attribute a read to a view-only identity rather than the dashboard ServiceAccount (PLANNING).

Pinning the grant forced a decision ADR-0013 deferred. Kubernetes requires `Impersonate-User` to be set to impersonate at all — a group cannot be impersonated alone. The viewer **group** pins cleanly (`resourceNames: [orkano:viewers]`, a fixed literal), but the **user** does not:

- If `Impersonate-User` is the authenticated human's username, the individual human lands in the K8s audit trail (PLANNING's stated intent) — but OIDC usernames are open-ended, so the `impersonate users` grant cannot be `resourceNames`-pinned. An unpinned `impersonate users` is the exact escalation surface ADR-0013 closed for groups: any user with direct admin bindings could be impersonated.
- If `Impersonate-User` is a single fixed identity, both `impersonate` verbs pin cleanly and there is no unpinned impersonation surface anywhere.

The trade-off is per-human attribution in the *K8s* audit log versus an airtight INV-01. Orkano keeps its own append-only audit log (INV-08) that already records the human for every privileged action, so the human identity is not lost either way.

## Decision

1. The dashboard impersonates a **fixed** identity for all read views: user `orkano:viewer` and group `orkano:viewers`, both `resourceNames`-pinned. The grant is the ClusterRole `orkano-dashboard-impersonate` (impersonate on `users` pinned to `orkano:viewer` and on `groups` pinned to `orkano:viewers`), bound to the dashboard ServiceAccount. `impersonate` on `users`/`groups` is cluster-scoped, so it is a ClusterRole — a namespaced Role cannot grant it. No `impersonate` verb is left unpinned; the dashboard can name no other identity, `system:masters` in particular.
2. The fixed group `orkano:viewers` is the only load-bearing identity. It is bound to the read-only `orkano-viewer` Role (a RoleBinding shipped in `config/rbac`, so impersonated reads work out of the box on a fresh single-admin cluster). The fixed user `orkano:viewer` holds no binding of its own; it exists only because impersonation requires a user.
3. The individual human is attributed in Orkano's own append-only audit log (INV-08), not the K8s audit trail, which sees the stable `orkano:viewer`/`orkano:viewers` identity. Writes — and the read legs of a write — stay on the dashboard ServiceAccount, so a mutation never depends on impersonation being configured.
4. `rbac_matrix_test.go` proves the pin binds: the allowed walk asserts the dashboard may impersonate exactly the pinned user and group, and an explicit wrong-name probe asserts it may impersonate nothing else (nameless, `system:masters`, or the cross-pinned name), since the cluster-grant suppression in the denied walk ignores `resourceNames`. A dedicated check asserts a member of `orkano:viewers` can read but not mutate in `orkano-apps`.

## Consequences

- INV-01 holds airtight and is now stated with impersonation included: every `impersonate` verb is `resourceNames`-pinned to a view-only identity, so a fully compromised dashboard can read as a viewer and write value-blind secrets but can escalate to no other identity.
- The K8s API audit trail attributes dashboard reads to `orkano:viewer`/`orkano:viewers`, not the individual operator. This is a deliberate departure from PLANNING's "the audit log sees the human": the human is recorded in Orkano's audit_log instead, and pinning every impersonate verb is worth more than per-human K8s-audit granularity for a single-admin v1. Multi-user, per-human K8s attribution can revisit this when OIDC and team RBAC arrive (it would require either an unpinned `impersonate users` or per-user RoleBindings).
- The viewer group binding ships in `config/rbac`, so impersonated reads are functional on install with no extra step. `rbac_matrix_test` learned to model a Group-subject binding (it previously assumed ServiceAccount subjects only).
- The fixed identity makes the viewer client a startup singleton — no per-request construction, no per-user state that a caching bug could leak between requests.

## Alternatives considered

- **Impersonate the session username + pinned group** — gives per-human K8s-audit attribution but requires an unpinned `impersonate users` grant, reopening (narrowly) the escalation hole ADR-0013 closed. Rejected: pinning every verb beats audit granularity here, and Orkano's own audit_log preserves the human.
- **Per-user RoleBindings minted on login** — pins per human but adds binding churn hostile to a solo maintainer and is unnecessary for a single-admin v1.
- **Bind the viewer group at install/OIDC time instead of in `config/rbac`** — keeps the matrix test simpler (no Group subject) but leaves impersonated reads returning 403 until an operator binds the group, contradicting "works out of the box."
- **No impersonation; read as the ServiceAccount** — simplest, but discards the entire point of impersonation: the cluster could not distinguish dashboard reads from any other SA action, weakening INV-01's auditability story.
