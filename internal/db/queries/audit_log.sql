-- name: AppendAuditEntry :exec
-- INV-08: the dashboard role holds INSERT and SELECT on audit_log but never
-- UPDATE or DELETE, so an entry can be added and shown but never rewritten or
-- erased. The write is fire-and-forget (no RETURNING) so the audit-WRITE path
-- needs only INSERT — the role's SELECT exists solely for ListAuditEntries (the
-- audit view). `detail` is structured context and per INV-03 never carries a
-- secret value.
INSERT INTO audit_log (actor, action, target, outcome, detail)
VALUES (@actor, @action, @target, @outcome, COALESCE(@detail::jsonb, '{}'::jsonb));

-- name: ListAuditEntries :many
-- Most-recent-first, paged by limit/offset for the audit view.
SELECT id, occurred_at, actor, action, target, outcome, detail
FROM audit_log
ORDER BY id DESC
LIMIT $1 OFFSET $2;
