-- +goose Up
-- M2.3 bootstrap-auth state for the dashboard's own credential store (ADR-0003):
-- account-lockout counters, a step-up re-auth marker on sessions, and single-use
-- recovery codes.
--
-- INV-03 boundary (same framing as 00003): there is deliberately NO column here
-- for a USER-APP secret value. The auth material this migration adds — the
-- failed-login/lockout counters and the SHA256 HASHES of recovery codes — is the
-- dashboard's OWN credential store, which ADR-0003 commits to keeping in Postgres.
-- That is a different category from the app secrets INV-03 protects (those live
-- only in Kubernetes Secrets), and a dumped database still yields zero user-app
-- credentials and no replayable recovery code.

-- Account lockout state. failed_logins counts consecutive failures; locked_until
-- holds the lockout deadline (NULL = not locked). ResetFailedLogins clears both
-- on a successful login.
ALTER TABLE users
    ADD COLUMN failed_logins integer NOT NULL DEFAULT 0,
    ADD COLUMN locked_until  timestamptz;

-- Step-up re-auth marker: set when the session most recently re-proved the second
-- factor, so a sensitive action can require a fresh re-auth. NULL = never stepped
-- up in this session.
ALTER TABLE sessions
    ADD COLUMN reauth_at timestamptz;

-- At most one CONFIRMED admin can ever exist (ADR-0003 single local admin). Every
-- confirmed row indexes the same constant key (TRUE), so a second confirmation
-- fails 23505 — the atomic backstop to the CountConfirmedAdmins gate against a
-- concurrent-redeem race (two enrollments confirming at once). Unconfirmed rows
-- (totp_confirmed_at IS NULL) are excluded by the partial WHERE.
CREATE UNIQUE INDEX users_single_confirmed_admin
    ON users ((totp_confirmed_at IS NOT NULL))
    WHERE totp_confirmed_at IS NOT NULL;

-- Single-use recovery codes (ADR-0003), the TOTP fallback. Stored as one-way
-- SHA256 HASHES of the codes shown once at enrollment — never the plaintext — so a
-- dumped database cannot be replayed to recover an account. A code is consumed by
-- stamping used_at; the UNIQUE(user_id, code_hash) makes a re-presented code a
-- no-op against an already-used row.
CREATE TABLE recovery_codes (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  text NOT NULL CHECK (char_length(code_hash) <= 128),
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, code_hash)
);
CREATE INDEX recovery_codes_user_id_idx ON recovery_codes (user_id);

-- Least-privilege grant: the dashboard role consumes (UPDATE = mark used) and
-- regenerates (DELETE) recovery codes. audit_log stays untouched (INV-08: still no
-- UPDATE/DELETE on it).
GRANT SELECT, INSERT, UPDATE, DELETE ON recovery_codes TO orkano_dashboard;

-- +goose Down
REVOKE ALL ON recovery_codes FROM orkano_dashboard;
DROP TABLE recovery_codes;
ALTER TABLE sessions DROP COLUMN reauth_at;
DROP INDEX IF EXISTS users_single_confirmed_admin;
ALTER TABLE users
    DROP COLUMN locked_until,
    DROP COLUMN failed_logins;
