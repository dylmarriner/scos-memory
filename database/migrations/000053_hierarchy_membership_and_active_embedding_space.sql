BEGIN;

-- 1. Hierarchy membership (§25.6).
--
-- hierarchy_nodes.fact_count was whatever number the caller put in the
-- request body: nothing tied a node to any fact. A node now names the facts
-- it organises, and its fact count is computed at read time from the facts
-- in its subtree that are still active (memory-intelligence loadHierarchy).
-- The stored column is dropped so nothing can read the caller's number back.
CREATE TABLE IF NOT EXISTS hierarchy_node_facts (
    node_id uuid NOT NULL REFERENCES hierarchy_nodes(id) ON DELETE CASCADE,
    memory_id uuid NOT NULL REFERENCES memory_items(id) ON DELETE CASCADE,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, memory_id)
);

CREATE INDEX IF NOT EXISTS hierarchy_node_facts_memory_idx ON hierarchy_node_facts (memory_id);

ALTER TABLE hierarchy_node_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE hierarchy_node_facts FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS hierarchy_node_facts_tenant_access ON hierarchy_node_facts;
CREATE POLICY hierarchy_node_facts_tenant_access ON hierarchy_node_facts FOR ALL
    USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

ALTER TABLE hierarchy_nodes DROP COLUMN IF EXISTS fact_count;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON hierarchy_node_facts TO scos_app;
    END IF;
END
$$;

-- 2. The active embedding space.
--
-- scos_worker_embedding_backlog() counted active memories with no embedding
-- in any space, so once a model change introduced a new space every memory
-- embedded under the old one read as "done" while retrieval, which only
-- compares vectors within the active space, could not use it. The embeddings
-- worker now records the space it writes as the active one for its role
-- (activated_at; the most recent activation per role wins), and the backlog
-- counts active memories whose current version has no chunk-0 vector in that
-- space.
ALTER TABLE embedding_spaces ADD COLUMN IF NOT EXISTS activated_at timestamptz;

-- Until the worker's next cycle records its space, treat the most recently
-- created text space that already holds vectors as active.
UPDATE embedding_spaces es
   SET activated_at = now()
 WHERE es.id = (
        SELECT s.id FROM embedding_spaces s
         WHERE s.role = 'text'
           AND EXISTS (SELECT 1 FROM memory_embeddings me WHERE me.embedding_space_id = s.id)
         ORDER BY s.created_at DESC, s.generation DESC
         LIMIT 1)
   AND NOT EXISTS (SELECT 1 FROM embedding_spaces a WHERE a.role = 'text' AND a.activated_at IS NOT NULL);

-- Same signature (and so the same grants) as migrations/000043. With no
-- active text space recorded, nothing is embedded in the active space, so
-- every active memory is backlog.
CREATE OR REPLACE FUNCTION scos_worker_embedding_backlog()
RETURNS bigint
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    WITH active_space AS (
        SELECT s.id FROM embedding_spaces s
         WHERE s.role = 'text' AND s.activated_at IS NOT NULL
         ORDER BY s.activated_at DESC
         LIMIT 1)
    SELECT count(*)
      FROM memory_items mi
      JOIN memory_versions mv ON mv.id = mi.current_version_id
     WHERE mi.status = 'active'
       AND NOT EXISTS (
            SELECT 1 FROM memory_embeddings me
             WHERE me.memory_version_id = mv.id
               AND me.embedding_space_id = (SELECT id FROM active_space)
               AND me.chunk_number = 0
               AND me.embedding IS NOT NULL)
$$;

REVOKE ALL ON FUNCTION scos_worker_embedding_backlog() FROM PUBLIC;

COMMIT;
