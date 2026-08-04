BEGIN;

-- Durable agent registry: replaces the service-state JSON blob previously
-- used by services/agent-registry. Identity, status and ordering are
-- relational; capability/permission lists remain jsonb per the same
-- convention used in 000009_governance_persistence.sql.
CREATE TABLE IF NOT EXISTS agent_registrations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name text NOT NULL,
    version text NOT NULL,
    maintainer text NOT NULL,
    capabilities jsonb NOT NULL DEFAULT '[]'::jsonb,
    inputs jsonb NOT NULL DEFAULT '[]'::jsonb,
    outputs jsonb NOT NULL DEFAULT '[]'::jsonb,
    tool_permissions jsonb NOT NULL DEFAULT '[]'::jsonb,
    memory_permissions jsonb NOT NULL DEFAULT '[]'::jsonb,
    max_classification text NOT NULL DEFAULT 'confidential',
    model_provider text NOT NULL,
    cost_policy text NOT NULL,
    health_endpoint text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    registered_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS agent_handoffs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    from_agent text NOT NULL,
    to_agent text NOT NULL,
    objective text NOT NULL,
    constraints jsonb NOT NULL DEFAULT '[]'::jsonb,
    authorised_namespaces jsonb NOT NULL DEFAULT '[]'::jsonb,
    memory_references jsonb NOT NULL DEFAULT '[]'::jsonb,
    artifact_references jsonb NOT NULL DEFAULT '[]'::jsonb,
    completed_work jsonb NOT NULL DEFAULT '[]'::jsonb,
    open_work jsonb NOT NULL DEFAULT '[]'::jsonb,
    known_failures jsonb NOT NULL DEFAULT '[]'::jsonb,
    acceptance_criteria text,
    expires_at timestamptz,
    status text NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS agent_registrations_tenant_idx ON agent_registrations (tenant_id, status);
CREATE INDEX IF NOT EXISTS agent_handoffs_tenant_idx ON agent_handoffs (tenant_id, status, created_at DESC);

COMMIT;
