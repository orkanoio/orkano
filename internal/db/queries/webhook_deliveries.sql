-- name: EnqueueDelivery :execrows
INSERT INTO webhook_deliveries (delivery_id, repo, event_type)
VALUES ($1, $2, $3)
ON CONFLICT (delivery_id) DO NOTHING;
