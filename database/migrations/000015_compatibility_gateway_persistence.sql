BEGIN;

-- Durable, tenant-scoped storage for context envelopes rendered/consumed by
-- the compatibility gateway (governance ss24). Session identifiers here are
-- caller-supplied external identifiers (not necessarily rows in `sessions`),
-- so this table keys on (tenant_id, session_id) rather than a foreign key
-- into the durable sessions/turns lineage.
CREATE TABLE IF NOT EXISTS context_envelopes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    session_id text NOT NULL,
    agent_id text NOT NULL DEFAULT '',
    envelope jsonb NOT NULL DEFAULT '{}'::jsonb,
    format text NOT NULL DEFAULT 'json',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, session_id)
);

COMMIT;
