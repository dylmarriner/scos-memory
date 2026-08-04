BEGIN;

-- security-gate previously persisted its entire state (audit log, secret
-- references, issued auth tokens) as one JSON blob per service instance via
-- the service_state table (whole-row last-write-wins, no per-item identity).
-- This gives it real relational homes: audit entries already have one
-- (audit_entries, migrations/000001_initial.sql); issued tokens and secret
-- references did not, so they are added here.

CREATE TABLE IF NOT EXISTS security_tokens (
    id text PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    principal_id uuid NOT NULL REFERENCES principals(id),
    agent_id text,
    session_id text,
    max_data_class text NOT NULL,
    issued_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE INDEX IF NOT EXISTS security_tokens_tenant_idx ON security_tokens (tenant_id, principal_id);

CREATE TABLE IF NOT EXISTS secret_references (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    ref_id text NOT NULL,
    name text NOT NULL,
    provider text NOT NULL,
    ref text NOT NULL,
    scope text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, ref_id)
);

COMMIT;
