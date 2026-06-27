-- +goose Up
-- The dashboard's own metadata store, in the SAME platform Postgres as the
-- webhook queue (one database, one migration sequence). Four tables: the local
-- admin account(s), opaque server-side sessions, the append-only audit log, and
-- the deploy timeline.
--
-- INV-03 boundary: there is deliberately NO column anywhere here for a USER-APP
-- secret value (env vars, catalog connection strings, the GitHub App PEM). Those
-- live only in Kubernetes Secrets. The auth material these tables DO hold —
-- bcrypt password hashes, the TOTP seed, hashed session ids — is the dashboard's
-- own credential store, which ADR-0003 commits to keeping in Postgres; that is a
-- different category from the app secrets INV-03 protects, and a dumped database
-- still yields zero user-app credentials.

-- The single local admin (single-tenant v1; the table generalizes to OIDC-linked
-- identities later — that migration adds the columns it needs). ADR-0003 admits
-- no state where an admin exists without a second factor, so password_hash and
-- totp_secret are both NOT NULL and set together at account creation. Lockout
-- counters and recovery codes are auth-flow state the M2.3 auth task adds.
CREATE TABLE users (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username          text NOT NULL CHECK (char_length(username) BETWEEN 1 AND 254),
    password_hash     text NOT NULL CHECK (char_length(password_hash) <= 255),
    totp_secret       text NOT NULL CHECK (char_length(totp_secret) <= 255),
    totp_confirmed_at timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
-- Usernames are matched case-insensitively, so uniqueness is enforced on the
-- lowercased form.
CREATE UNIQUE INDEX users_username_lower_key ON users (lower(username));

-- Opaque server-side sessions (ADR-0003 — deliberately not stateless JWTs, so an
-- admin session is revocable instantly). The raw cookie token is NEVER stored:
-- the row is keyed by sha256(token), so a dumped database cannot be replayed as a
-- live session. expires_at is the hard lifetime cap; last_used_at slides on use.
CREATE TABLE sessions (
    token_hash   text PRIMARY KEY CHECK (char_length(token_hash) <= 128),
    user_id      bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_used_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- INV-08: every privileged action lands here, and the audit log is append-only.
-- Immutability is enforced at the role level (00004 grants the dashboard role
-- INSERT + SELECT but never UPDATE/DELETE), so even a fully compromised dashboard
-- can add and display entries but never rewrite or erase history. `detail` is
-- structured context and per INV-03 never carries a secret value.
CREATE TABLE audit_log (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    actor       text NOT NULL CHECK (char_length(actor) <= 254),
    action      text NOT NULL CHECK (char_length(action) <= 100),
    target      text NOT NULL DEFAULT '' CHECK (char_length(target) <= 512),
    outcome     text NOT NULL CHECK (char_length(outcome) <= 32),
    detail      jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_occurred_at_idx ON audit_log (occurred_at);

-- The per-app deploy timeline. Build CRs get pruned/GC'd, so the durable history
-- the App UI shows lives here. Like the audit log it is append-only by intent (a
-- deploy event is immutable), but it is operational rather than security record,
-- so it is not covered by INV-08's no-UPDATE/DELETE guarantee.
CREATE TABLE deploy_history (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    app_namespace text NOT NULL CHECK (char_length(app_namespace) <= 253),
    app_name      text NOT NULL CHECK (char_length(app_name) <= 253),
    build_name    text NOT NULL DEFAULT '' CHECK (char_length(build_name) <= 253),
    image         text NOT NULL DEFAULT '' CHECK (char_length(image) <= 512),
    status        text NOT NULL CHECK (char_length(status) <= 64)
);
CREATE INDEX deploy_history_app_idx ON deploy_history (app_namespace, app_name, id);

-- +goose Down
DROP TABLE deploy_history;
DROP TABLE audit_log;
DROP TABLE sessions;
DROP TABLE users;
