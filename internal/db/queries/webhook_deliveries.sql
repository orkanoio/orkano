-- name: EnqueueDelivery :execrows
-- Bare ON CONFLICT DO NOTHING, deliberately with no named arbiter: naming the
-- conflict target (delivery_id) makes Postgres infer the arbiter index, which
-- requires SELECT on that column — a privilege the INSERT-only receiver role
-- must never hold (INV-04). delivery_id is the table's only unique constraint,
-- so the bare form dedups identically while keeping the receiver INSERT-only.
INSERT INTO webhook_deliveries (delivery_id, repo, event_type)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: ClaimDelivery :one
-- The dispatcher's consume half: claim the oldest doorbell, skipping any row a
-- concurrent claim already holds. FOR UPDATE locks the row until the surrounding
-- transaction ends, so the dispatcher can act on the delivery (re-fetch the
-- commit, create the Build) and only then DELETE + COMMIT — at-least-once
-- delivery, made exactly-once by the Build's deterministic name. SKIP LOCKED
-- keeps a single stuck delivery from blocking the rest (and is correct if a
-- second consumer ever appears). No rows -> pgx.ErrNoRows = queue drained.
SELECT id, delivery_id, repo, event_type
FROM webhook_deliveries
ORDER BY id
FOR UPDATE SKIP LOCKED
LIMIT 1;

-- name: DeleteDelivery :exec
-- Remove a processed doorbell. Run inside the same transaction as ClaimDelivery
-- so the lock is released by the COMMIT that also deletes the row.
DELETE FROM webhook_deliveries WHERE id = $1;
