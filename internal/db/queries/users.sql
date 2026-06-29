-- name: CreateUser :one
-- The bootstrap admin (single-tenant v1). Created in one shot with the bcrypt
-- hash and the TOTP seed already set — ADR-0003 admits no state where an admin
-- exists without a second factor.
INSERT INTO users (username, password_hash, totp_secret, totp_confirmed_at)
VALUES ($1, $2, $3, $4)
RETURNING id, username, password_hash, totp_secret, totp_confirmed_at, failed_logins, locked_until, created_at, updated_at;

-- name: GetUserByUsername :one
-- Login lookup; matched case-insensitively against the lowercased unique index.
-- Returns the lockout state so the handler can refuse a locked account, plus
-- oidc_subject so the password path can treat an OIDC identity like an unknown
-- user (ADR-0016 §6).
SELECT id, username, password_hash, totp_secret, totp_confirmed_at, failed_logins, locked_until, oidc_issuer, oidc_subject, created_at, updated_at
FROM users
WHERE lower(username) = lower(@username);

-- name: GetUserByID :one
-- oidc_subject lets resolveSession admit an OIDC-linked row on the positive
-- subject signal (ADR-0016 §6), never merely on a NULL totp_confirmed_at.
SELECT id, username, password_hash, totp_secret, totp_confirmed_at, failed_logins, locked_until, oidc_issuer, oidc_subject, created_at, updated_at
FROM users
WHERE id = $1;

-- name: GetUserByOIDC :one
-- The OIDC login lookup keyed on the real identity (issuer, subject). A hit means
-- the human already has a JIT row; a miss (pgx.ErrNoRows) means provision one.
SELECT id, username, password_hash, totp_secret, totp_confirmed_at, failed_logins, locked_until, oidc_issuer, oidc_subject, created_at, updated_at
FROM users
WHERE oidc_issuer = @issuer AND oidc_subject = @subject;

-- name: CreateOIDCUser :one
-- Just-in-time provision an OIDC-linked user (ADR-0016 §5): a credential-less
-- session anchor. password_hash and totp_secret are set to '' on purpose (the
-- credential + MFA live at the IdP); totp_confirmed_at stays NULL so the
-- single-confirmed-admin index ignores it. username holds the IdP email for
-- display/audit; a collision trips the lower(username) unique index (23505).
-- RETURNING is minimal — the id is all the caller needs to mint a session; read
-- the full anchor via GetUserByOIDC/GetUserByID if more is ever required.
INSERT INTO users (username, password_hash, totp_secret, oidc_issuer, oidc_subject)
VALUES (@username, '', '', @issuer, @subject)
RETURNING id, username, oidc_issuer, oidc_subject;

-- name: CountUsers :one
-- A coarse population count. NOTE the real bootstrap gate is CountConfirmedAdmins,
-- not this — post-OIDC (00006) a JIT OIDC user counts here too, so CountUsers > 0
-- no longer implies the local admin exists.
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
-- Clear an abandoned LOCAL-ADMIN enrollment (a row created but TOTP never
-- confirmed) before a fresh install-token redeem. Not a bypass: the install token
-- is still required per redeem attempt. An OIDC row (00006) is ALSO
-- totp_confirmed_at NULL but is a permanent identity anchor, not an abandoned
-- enrollment — `oidc_subject IS NULL` excludes it so a redeem never wipes an OIDC
-- user (ADR-0016 §5).
DELETE FROM users WHERE totp_confirmed_at IS NULL AND oidc_subject IS NULL;
