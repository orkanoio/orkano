-- +goose Up
-- The dashboard's least-privilege Postgres role. Like the receiver/dispatcher
-- roles (00002), it is created here with the privilege SHAPE only — no password.
-- The install sets the password via ALTER ROLE (db.SetupRoles) so the credential
-- lives only in the install-generated Secrets, never in version control.
--
-- INV-08 (append-only audit) is enforced HERE: the role may INSERT into and
-- SELECT from audit_log (write an entry, render the audit view) but holds no
-- UPDATE or DELETE on it, so a fully compromised dashboard can neither rewrite
-- nor erase history. It gets full CRUD on its own account/session tables and
-- append+read on deploy_history. It is granted nothing on the webhook queue
-- tables, so the dashboard role cannot read or drain the queue.

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'orkano_dashboard') THEN
        CREATE ROLE orkano_dashboard LOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO orkano_dashboard;

GRANT SELECT, INSERT, UPDATE, DELETE ON users TO orkano_dashboard;
GRANT SELECT, INSERT, UPDATE, DELETE ON sessions TO orkano_dashboard;
GRANT SELECT, INSERT ON deploy_history TO orkano_dashboard;
-- audit_log: append + read only. No UPDATE/DELETE — the INV-08 guarantee.
GRANT SELECT, INSERT ON audit_log TO orkano_dashboard;

-- +goose Down
REVOKE ALL ON users, sessions, audit_log, deploy_history FROM orkano_dashboard;
REVOKE USAGE ON SCHEMA public FROM orkano_dashboard;
DROP ROLE IF EXISTS orkano_dashboard;
