-- name: InsertRecoveryCode :exec
-- Store one recovery code as its SHA256 hash (never plaintext). The caller hashes
-- the code it showed once at enrollment.
INSERT INTO recovery_codes (user_id, code_hash) VALUES ($1, $2);

-- name: ConsumeRecoveryCode :one
-- Single-use redemption: stamp used_at on a still-unused code for this user. A row
-- returned means it was consumed; pgx.ErrNoRows means the code is invalid or was
-- already used. The WHERE used_at IS NULL makes a replay of a spent code a no-op.
UPDATE recovery_codes
SET used_at = now()
WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL
RETURNING id;

-- name: CountUnusedRecoveryCodes :one
-- How many recovery codes remain for a user (drives a low-codes warning / forced
-- regeneration).
SELECT count(*) FROM recovery_codes WHERE user_id = $1 AND used_at IS NULL;
