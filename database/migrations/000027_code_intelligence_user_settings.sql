BEGIN;

-- Per-user toggle for the durable, snapshot-based code store (v2 routes;
-- see pgstore.go/pghandlers.go). The store itself is always running --
-- DATABASE_URL is required at startup, matching every other Postgres-backed
-- service in this fleet -- but an individual user can still opt out of the
-- v2 routes from the dashboard settings tab. Absence of a row means
-- enabled: the default is "on", and this table only ever records an
-- explicit opt-out, never the common case.
CREATE TABLE IF NOT EXISTS code_intelligence_user_settings (
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    principal_id text NOT NULL,
    durable_store_enabled boolean NOT NULL DEFAULT true,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, principal_id)
);

COMMIT;
