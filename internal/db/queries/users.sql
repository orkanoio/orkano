-- name: CreateUser :one
-- The bootstrap admin (single-tenant v1). Created in one shot with the bcrypt
-- hash and the TOTP seed already set — ADR-0003 admits no state where an admin
-- exists without a second factor.
INSERT INTO users (username, password_hash, totp_secret, totp_confirmed_at)
VALUES ($1, $2, $3, $4)
RETURNING id, username, password_hash, totp_secret, totp_confirmed_at, created_at, updated_at;

-- name: GetUserByUsername :one
-- Login lookup; matched case-insensitively against the lowercased unique index.
SELECT id, username, password_hash, totp_secret, totp_confirmed_at, created_at, updated_at
FROM users
WHERE lower(username) = lower($1);

-- name: GetUserByID :one
SELECT id, username, password_hash, totp_secret, totp_confirmed_at, created_at, updated_at
FROM users
WHERE id = $1;

-- name: CountUsers :one
-- The bootstrap-completed check: zero users means the one-time install token is
-- still redeemable; one or more means the local admin already exists.
SELECT count(*) FROM users;
