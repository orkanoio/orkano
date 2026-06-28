-- name: CreateUser :one
-- The bootstrap admin (single-tenant v1). Created in one shot with the bcrypt
-- hash and the TOTP seed already set — ADR-0003 admits no state where an admin
-- exists without a second factor.
INSERT INTO users (username, password_hash, totp_secret, totp_confirmed_at)
VALUES ($1, $2, $3, $4)
RETURNING id, username, password_hash, totp_secret, totp_confirmed_at, failed_logins, locked_until, created_at, updated_at;

-- name: GetUserByUsername :one
-- Login lookup; matched case-insensitively against the lowercased unique index.
-- Returns the lockout state so the handler can refuse a locked account.
SELECT id, username, password_hash, totp_secret, totp_confirmed_at, failed_logins, locked_until, created_at, updated_at
FROM users
WHERE lower(username) = lower(@username);

-- name: GetUserByID :one
SELECT id, username, password_hash, totp_secret, totp_confirmed_at, failed_logins, locked_until, created_at, updated_at
FROM users
WHERE id = $1;

-- name: CountUsers :one
-- The bootstrap-completed check: zero users means the one-time install token is
-- still redeemable; one or more means the local admin already exists.
SELECT count(*) FROM users;

-- name: CountConfirmedAdmins :one
-- Redemption of the install token is open only while this is 0: a confirmed admin
-- (second factor enrolled) means bootstrap is done.
SELECT count(*) FROM users WHERE totp_confirmed_at IS NOT NULL;

-- name: IncrementFailedLogins :one
-- Bump the consecutive-failure counter on a bad attempt; returns the new count so
-- the handler can lock once it crosses the threshold.
UPDATE users
SET failed_logins = failed_logins + 1, updated_at = now()
WHERE id = $1
RETURNING failed_logins;

-- name: LockUser :exec
-- Lock the account until @locked_until (the handler computes the deadline).
UPDATE users SET locked_until = @locked_until, updated_at = now() WHERE id = @user_id;

-- name: ResetFailedLogins :exec
-- Clear the lockout state on a successful login.
UPDATE users SET failed_logins = 0, locked_until = NULL, updated_at = now() WHERE id = $1;

-- name: ConfirmUserTOTP :exec
-- Stamp the second factor as enrolled, completing bootstrap.
UPDATE users SET totp_confirmed_at = now(), updated_at = now() WHERE id = $1;

-- name: DeleteUnconfirmedUsers :exec
-- Clear an abandoned enrollment (a user row created but TOTP never confirmed)
-- before a fresh install-token redeem. Not a bypass: the install token is still
-- required per redeem attempt.
DELETE FROM users WHERE totp_confirmed_at IS NULL;
