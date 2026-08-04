BEGIN;

-- The organisational hierarchy (memory palace / wing / drawer / closet /
-- diary / artifact, §25.6) has no durable home in earlier migrations. Every
-- other memory-intelligence plane (facts, entities, beliefs, diary) already
-- has a relational table from 000001/000009; this is the one addition that
-- table set is missing.
CREATE TABLE IF NOT EXISTS hierarchy_nodes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    level text NOT NULL DEFAULT 'drawer',
    name text NOT NULL,
    parent_id uuid REFERENCES hierarchy_nodes(id) ON DELETE SET NULL,
    scope text NOT NULL DEFAULT 'global',
    summary text,
    fact_count integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS hierarchy_nodes_tenant_idx ON hierarchy_nodes (tenant_id, parent_id);

COMMIT;
