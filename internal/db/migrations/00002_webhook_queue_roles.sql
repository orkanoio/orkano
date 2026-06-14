-- +goose Up
-- Least-privilege Postgres roles for the webhook queue.
--
-- orkano_receiver is the internet-facing receiver: INSERT and nothing else, not
-- even SELECT on its own table. A compromised receiver can enqueue doorbells but
-- can never read, mutate, or drain the queue (INV-04 — the seed of the
-- webhook.receiver-blast-radius doctor check).
--
-- orkano_dispatcher is the operator's consumer: SELECT … FOR UPDATE SKIP LOCKED
-- then remove. It cannot INSERT, so the read and write halves of the event path
-- hold disjoint, minimal grants.
--
-- Passwords are deliberately NOT set here — init generates them and applies them
-- with ALTER ROLE at install. The migration ships only the privilege shape, which
-- is what belongs in version control.

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'orkano_receiver') THEN
        CREATE ROLE orkano_receiver LOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'orkano_dispatcher') THEN
        CREATE ROLE orkano_dispatcher LOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO orkano_receiver, orkano_dispatcher;

GRANT INSERT ON webhook_deliveries TO orkano_receiver;
GRANT SELECT, UPDATE, DELETE ON webhook_deliveries TO orkano_dispatcher;

-- +goose Down
REVOKE ALL ON webhook_deliveries FROM orkano_receiver, orkano_dispatcher;
REVOKE USAGE ON SCHEMA public FROM orkano_receiver, orkano_dispatcher;
DROP ROLE IF EXISTS orkano_receiver;
DROP ROLE IF EXISTS orkano_dispatcher;
