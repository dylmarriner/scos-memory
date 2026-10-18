BEGIN;

-- Synthesis proposes patterns for review as memory_candidates rows, which
-- row-level security admits only from a namespace member allowed to write
-- candidates. The acting principal is therefore such a member: the owner
-- when the owner holds that membership, otherwise the member with the most
-- rights. A namespace with no such member is not synthesised (the worker
-- skips rows whose acting principal is NULL).
CREATE OR REPLACE FUNCTION scos_worker_synthesis_namespaces(p_min_facts integer)
RETURNS TABLE (namespace_id uuid, tenant_id uuid, acting_principal_id uuid, fact_count bigint)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT mi.namespace_id, mi.tenant_id,
           (SELECT m.principal_id FROM namespace_memberships m
             WHERE m.namespace_id = n.id AND m.can_read AND m.can_write_candidates
             ORDER BY (m.principal_id = n.owner_principal_id) DESC, m.can_administer DESC, m.principal_id
             LIMIT 1),
           count(*)
      FROM memory_items mi
      JOIN namespaces n ON n.id = mi.namespace_id
     WHERE mi.status = 'active'
     GROUP BY mi.namespace_id, mi.tenant_id, n.id, n.owner_principal_id
    HAVING count(*) >= p_min_facts
$$;

COMMIT;
