BEGIN;

-- Tool catalog for the Tool Governance service (§28). A tool is registered
-- once (proposed) and then transitions through the shared operation_status
-- lifecycle before any operation may reference it. operations.tool_name /
-- operations.tool_version stay free-text (an operation can target a tool
-- version that has since been superseded), so the catalog is a separate,
-- append-safe table rather than a foreign key target.
CREATE TABLE IF NOT EXISTS tool_definitions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    version text NOT NULL,
    properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    state operation_status NOT NULL DEFAULT 'proposed',
    owner_principal_id uuid REFERENCES principals(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS tool_definitions_tenant_state_idx ON tool_definitions (tenant_id, state);

COMMIT;
