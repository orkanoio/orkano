-- +goose Up
-- The onboarding wizard's setup-state store: small non-secret key-value rows
-- (the chosen access mode, the "GitHub App connected" marker with the app slug
-- and id, the "OIDC written, restart pending" timestamp). It exists because the
-- dashboard is deliberately value-blind on the credential Secrets it writes
-- (update-only, no get — ADR-0013), so "connected" cannot be derived from the
-- Secret itself; the connect callback records the fact here instead.
--
-- INV-03: values are POINTERS and choices only — a mode name, an App slug, a
-- numeric App id, a timestamp. Never a credential (the PEM, webhook secret and
-- OIDC client secret live only in Kubernetes Secrets). The key ENUM is the
-- schema-level backstop: a length cap alone would still admit a short secret
-- under a novel key, so any new settings key needs a migration — the INV-03
-- decision is made visibly, at change time, not by review vigilance. The
-- dashboard_test column guard pins the shape.
CREATE TABLE settings (
    key        text PRIMARY KEY CHECK (key IN (
                   'access_mode',
                   'github_app_slug',
                   'github_app_id',
                   'github_connected_at',
                   'oidc_configured_at'
               )),
    value      text NOT NULL CHECK (char_length(value) <= 512),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Read + upsert only. No DELETE: setup state is re-chosen (upserted), never
-- erased, and withholding DELETE keeps the grant as small as the queries need.
GRANT SELECT, INSERT, UPDATE ON settings TO orkano_dashboard;

-- +goose Down
REVOKE ALL ON settings FROM orkano_dashboard;
DROP TABLE settings;
