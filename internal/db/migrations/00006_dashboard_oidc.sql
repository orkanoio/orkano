-- +goose Up
-- M2.5 OIDC sign-in (ADR-0016). The users table generalizes from "the single
-- local admin" to also hold OIDC-linked identities, exactly as migration 00003
-- foresaw ("that migration adds the columns it needs").
--
-- INV-03 / "not an identity provider" boundary: oidc_subject and oidc_issuer are
-- a POINTER to an identity that lives at the IdP — a stable, non-secret
-- identifier — NEVER a credential. The OIDC client secret and the user's MFA stay
-- outside Postgres (the client secret in a K8s Secret, the second factor at the
-- IdP). A dumped database still yields zero usable credentials.

-- An OIDC-linked user records (oidc_issuer, oidc_subject); a local-admin row
-- leaves both NULL. The CHECKs bound the lengths and require the pair to be set
-- together (a subject without an issuer is not a meaningful identity). A
-- just-in-time provisioned OIDC user carries no password hash and no TOTP seed
-- (password_hash = '' / totp_secret = '' / totp_confirmed_at NULL), the deliberate
-- exception to 00003's "always a bcrypt hash + TOTP seed" note (ADR-0016 §5): the
-- credential and MFA are the IdP's, so no future CHECK may force those columns
-- non-empty without excluding OIDC rows.
ALTER TABLE users
    ADD COLUMN oidc_issuer  text CHECK (oidc_issuer  IS NULL OR char_length(oidc_issuer)  BETWEEN 1 AND 512),
    ADD COLUMN oidc_subject text CHECK (oidc_subject IS NULL OR char_length(oidc_subject) BETWEEN 1 AND 255),
    ADD CONSTRAINT users_oidc_pair_together CHECK ((oidc_issuer IS NULL) = (oidc_subject IS NULL));

-- (issuer, subject) is the real identity key: the login lookup-or-create keys on
-- it, so a re-login is idempotent and two distinct IdP accounts can never collide.
-- Partial so the many local-admin / unconfirmed rows (both NULL) are unconstrained
-- and the single-confirmed-admin index (00005) is untouched.
CREATE UNIQUE INDEX users_oidc_identity_key
    ON users (oidc_issuer, oidc_subject)
    WHERE oidc_subject IS NOT NULL;

-- No new GRANT: the orkano_dashboard role already holds full CRUD on users
-- (00004), and column privileges follow the table grant.

-- +goose Down
DROP INDEX IF EXISTS users_oidc_identity_key;
ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_oidc_pair_together,
    DROP COLUMN oidc_subject,
    DROP COLUMN oidc_issuer;
