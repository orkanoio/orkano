-- +goose Up
-- The webhook delivery queue. Each row is a doorbell, not data: the receiver
-- records only that a delivery arrived so the dispatcher can re-fetch the
-- authoritative commit from the GitHub API. There is deliberately no payload
-- column — nothing the internet-facing webhook sends is ever trusted later
-- (INV-03, INV-04).
-- The text columns carry attacker-influenced values (delivery_id is the
-- X-GitHub-Delivery header; repo and event_type come off the wire), so each is
-- length-bounded to keep a malformed sender from bloating the table or the
-- unique index. The bounds are generous headroom over GitHub's real shapes
-- (UUID delivery ids, owner/name repos, short event names).
CREATE TABLE webhook_deliveries (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    delivery_id text NOT NULL CHECK (char_length(delivery_id) <= 72),
    repo        text NOT NULL CHECK (char_length(repo) <= 200),
    event_type  text NOT NULL CHECK (char_length(event_type) <= 50),
    received_at timestamptz NOT NULL DEFAULT now()
);

-- Duplicate deliveries (GitHub retries the same X-GitHub-Delivery) collapse to a
-- single row via ON CONFLICT DO NOTHING on enqueue — the idempotency seed the
-- dispatcher relies on.
CREATE UNIQUE INDEX webhook_deliveries_delivery_id_key ON webhook_deliveries (delivery_id);

-- +goose Down
DROP TABLE webhook_deliveries;
