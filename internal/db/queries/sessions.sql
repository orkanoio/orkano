-- name: CreateSession :exec
-- token_hash is sha256(opaque cookie token); the raw token is never stored. The
-- caller already holds everything it needs (it minted the raw token and sets the
-- cookie from it), so no RETURNING.
INSERT INTO sessions (token_hash, user_id, expires_at)
VALUES ($1, $2, $3);

-- name: GetSession :one
-- Resolve an opaque session by the hash of its cookie token, only while
-- unexpired, returning the owning user id so the request runs as that identity.
-- An expired (or revoked) row yields no rows -> pgx.ErrNoRows.
SELECT token_hash, user_id, created_at, expires_at, last_used_at
FROM sessions
WHERE token_hash = $1 AND expires_at > now();

-- name: TouchSession :exec
-- Slide the idle clock on use; expires_at stays the hard lifetime cap.
UPDATE sessions SET last_used_at = now() WHERE token_hash = $1;

-- name: DeleteSession :exec
-- Logout / instant revocation (ADR-0003): the next request with this cookie
-- finds no row and is rejected.
DELETE FROM sessions WHERE token_hash = $1;

-- name: DeleteUserSessions :exec
-- Revoke every session for a user (password change, admin lockout).
DELETE FROM sessions WHERE user_id = $1;

-- name: DeleteExpiredSessions :execrows
-- Periodic sweep of expired rows; returns the count purged.
DELETE FROM sessions WHERE expires_at <= now();
