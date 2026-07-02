-- name: GetSetting :one
-- One setup-state row by key; pgx.ErrNoRows when the key was never written.
SELECT key, value, updated_at
FROM settings
WHERE key = $1;

-- name: UpsertSetting :exec
-- Write-or-overwrite one setup-state row. The named arbiter is safe here (the
-- dashboard role holds SELECT on settings, unlike the receiver's INSERT-only
-- role that forced the bare form in webhook_deliveries).
INSERT INTO settings (key, value, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: ListSettings :many
-- The whole setup-state map (small by construction); the wizard status endpoint
-- reads it in one round trip.
SELECT key, value, updated_at
FROM settings
ORDER BY key;
