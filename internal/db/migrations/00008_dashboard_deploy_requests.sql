-- +goose Up
-- Manual deploys use the same trusted-pointer queue as GitHub pushes, but they
-- target one App rather than fanning out to every App backed by the repository.
-- app_name is still a pointer, never webhook payload or source data. The
-- named constraint makes the two producer lanes disjoint: receiver rows are
-- repo-wide GitHub pushes; dashboard rows are well-formed, app-scoped manual
-- requests. This remains true even if either process is compromised.
ALTER TABLE webhook_deliveries
    ADD COLUMN app_name text,
    ADD CONSTRAINT webhook_deliveries_scope_check CHECK (
        (event_type = 'push' AND app_name IS NULL)
        OR
        (event_type = 'manual'
            AND delivery_id ~ '^manual-[0-9a-f]{32}$'
            AND app_name IS NOT NULL
            AND char_length(app_name) BETWEEN 1 AND 253
            AND app_name ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$')
    );

-- Migration 00002 granted table-level INSERT before app_name existed. Replace
-- it with a column grant so the internet-facing receiver cannot populate the
-- newly-added app selector; the scope constraint then rejects a forged manual
-- event with app_name omitted.
REVOKE INSERT ON webhook_deliveries FROM orkano_receiver;
GRANT INSERT (delivery_id, repo, event_type) ON webhook_deliveries TO orkano_receiver;

-- The dashboard may ring one app-scoped doorbell and nothing more: no SELECT,
-- UPDATE, DELETE, or TRUNCATE on the queue. The dispatcher remains the only
-- consumer and still resolves the authoritative commit through GitHub.
GRANT INSERT (delivery_id, repo, event_type, app_name) ON webhook_deliveries TO orkano_dashboard;

-- +goose Down
REVOKE INSERT (delivery_id, repo, event_type, app_name) ON webhook_deliveries FROM orkano_dashboard;
REVOKE INSERT (delivery_id, repo, event_type) ON webhook_deliveries FROM orkano_receiver;
GRANT INSERT ON webhook_deliveries TO orkano_receiver;
ALTER TABLE webhook_deliveries DROP CONSTRAINT webhook_deliveries_scope_check;
ALTER TABLE webhook_deliveries DROP COLUMN app_name;
