-- name: EnqueueDelivery :execrows
-- Bare ON CONFLICT DO NOTHING, deliberately with no named arbiter: naming the
-- conflict target (delivery_id) makes Postgres infer the arbiter index, which
-- requires SELECT on that column — a privilege the INSERT-only receiver role
-- must never hold (INV-04). delivery_id is the table's only unique constraint,
-- so the bare form dedups identically while keeping the receiver INSERT-only.
INSERT INTO webhook_deliveries (delivery_id, repo, event_type)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;
