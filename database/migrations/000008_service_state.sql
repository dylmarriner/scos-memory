BEGIN;

CREATE TABLE IF NOT EXISTS service_state (
    service text PRIMARY KEY,
    state jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

COMMIT;
