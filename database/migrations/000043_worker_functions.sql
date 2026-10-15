BEGIN;

-- Fabric workers under the restricted runtime role.
--
-- The consolidation, embeddings, index-maintenance and policy-evaluation
-- workers connect as scos_app. Their work starts with a cross-tenant scan
-- (which outbox jobs are pending, which memory versions lack vectors, which
-- items have expired), but every table involved is under FORCE RLS keyed on
-- a tenant or principal context, so each scan saw nothing and the workers
-- silently did no work. These SECURITY DEFINER functions perform only those
-- cross-tenant steps; per-tenant processing then runs through the ordinary
-- policies with the tenant/principal of each job set in its transaction.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = current_user AND (rolsuper OR rolbypassrls)) THEN
        RAISE EXCEPTION 'worker functions must be created by a role with SUPERUSER or BYPASSRLS (current role: %)', current_user;
    END IF;
END
$$;

-- Claim up to p_limit due jobs of the given types for p_owner, leasing them
-- for p_lease_seconds. Each job is returned with the principal it runs as:
-- the payload's principal_id, or the actor of the turn it concerns.
CREATE OR REPLACE FUNCTION scos_worker_claim_outbox_jobs(p_owner text, p_job_types text[], p_limit integer, p_lease_seconds integer)
RETURNS TABLE (id uuid, tenant_id uuid, operation_id text, job_type text, payload jsonb, attempts integer, principal_id uuid)
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF p_limit < 1 OR p_limit > 500 OR p_lease_seconds < 1 OR p_lease_seconds > 3600 THEN
        RAISE EXCEPTION 'claim limit must be 1-500 and lease 1-3600 seconds';
    END IF;
    RETURN QUERY
    WITH claimed AS (
        UPDATE outbox_jobs j
           SET status = 'processing',
               lease_owner = p_owner,
               lease_expires_at = now() + make_interval(secs => p_lease_seconds),
               attempts = j.attempts + 1,
               updated_at = now()
         WHERE j.id IN (
                SELECT c.id FROM outbox_jobs c
                 WHERE c.status IN ('pending', 'retryable')
                   AND c.job_type = ANY (p_job_types)
                   AND c.available_at <= now()
                   AND (c.lease_expires_at IS NULL OR c.lease_expires_at < now())
                 ORDER BY c.available_at ASC
                 LIMIT p_limit
                 FOR UPDATE SKIP LOCKED)
        RETURNING j.id, j.tenant_id, j.operation_id, j.job_type, j.payload, j.attempts
    )
    SELECT c.id, c.tenant_id, c.operation_id, c.job_type, c.payload, c.attempts,
           COALESCE(
               CASE WHEN (c.payload->>'principal_id') ~* '^[0-9a-f-]{36}$' THEN (c.payload->>'principal_id')::uuid END,
               (SELECT t.actor_id FROM turns t WHERE t.id::text = c.payload->>'turn_id')
           )
      FROM claimed c;
END
$$;

-- Namespaces with enough active facts for pattern synthesis, each with the
-- principal synthesis writes as: the owner, else a member allowed to write.
CREATE OR REPLACE FUNCTION scos_worker_synthesis_namespaces(p_min_facts integer)
RETURNS TABLE (namespace_id uuid, tenant_id uuid, acting_principal_id uuid, fact_count bigint)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT mi.namespace_id, mi.tenant_id,
           COALESCE(n.owner_principal_id,
                    (SELECT m.principal_id FROM namespace_memberships m
                      WHERE m.namespace_id = n.id AND m.can_read AND m.can_write_candidates
                      ORDER BY m.can_administer DESC, m.principal_id LIMIT 1)),
           count(*)
      FROM memory_items mi
      JOIN namespaces n ON n.id = mi.namespace_id
     WHERE mi.status = 'active'
     GROUP BY mi.namespace_id, mi.tenant_id, n.id, n.owner_principal_id
    HAVING count(*) >= p_min_facts
$$;

-- Memory versions with no vector in the given embedding space.
CREATE OR REPLACE FUNCTION scos_worker_unembedded_versions(p_space_id uuid, p_limit integer)
RETURNS TABLE (id uuid, content text)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT mv.id, mv.content
      FROM memory_versions mv
     WHERE NOT EXISTS (
            SELECT 1 FROM memory_embeddings me
             WHERE me.memory_version_id = mv.id
               AND me.embedding_space_id = p_space_id
               AND me.chunk_number = 0
               AND me.embedding IS NOT NULL)
     ORDER BY mv.created_at ASC
     LIMIT LEAST(GREATEST(p_limit, 1), 1000)
$$;

-- Store (or replace) the chunk-0 vector of one memory version. The vector
-- is a derived, rebuildable projection of content the version already holds.
CREATE OR REPLACE FUNCTION scos_worker_store_embedding(
    p_version_id uuid, p_space_id uuid, p_provider text, p_model text, p_revision text,
    p_dimensions integer, p_content_hash text, p_embedding text)
RETURNS void
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    INSERT INTO memory_embeddings
        (memory_version_id, embedding_space_id, provider, model, model_revision,
         dimensions, chunk_number, chunk_content_hash, embedding)
    VALUES (p_version_id, p_space_id, p_provider, p_model, p_revision, p_dimensions, 0, p_content_hash, p_embedding::vector)
    ON CONFLICT (memory_version_id, embedding_space_id, chunk_number)
    DO UPDATE SET embedding = EXCLUDED.embedding,
                  provider = EXCLUDED.provider,
                  model = EXCLUDED.model,
                  model_revision = EXCLUDED.model_revision,
                  dimensions = EXCLUDED.dimensions,
                  chunk_content_hash = EXCLUDED.chunk_content_hash,
                  created_at = now()
$$;

-- Expire active memories whose explicit expiry has passed, then apply
-- memory_items retention rules (positive integer policy.ttl_days) to active
-- items without an explicit expiry. Returns (explicit, retention) counts.
CREATE OR REPLACE FUNCTION scos_worker_expire_memories(p_now timestamptz)
RETURNS TABLE (expired_explicit integer, expired_retention integer)
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    explicit_count integer;
    retention_count integer := 0;
    rule record;
    ttl integer;
    n integer;
BEGIN
    UPDATE memory_items SET status = 'expired'
     WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at < p_now;
    GET DIAGNOSTICS explicit_count = ROW_COUNT;

    FOR rule IN SELECT r.tenant_id, r.namespace_id, r.policy FROM retention_rules r WHERE r.resource_type = 'memory_items' LOOP
        IF jsonb_typeof(rule.policy->'ttl_days') <> 'number' THEN
            CONTINUE;
        END IF;
        ttl := (rule.policy->>'ttl_days')::numeric::integer;
        IF ttl IS NULL OR ttl <= 0 OR (rule.policy->>'ttl_days')::numeric <> ttl THEN
            CONTINUE;
        END IF;
        UPDATE memory_items SET status = 'expired'
         WHERE status = 'active' AND expires_at IS NULL
           AND tenant_id = rule.tenant_id
           AND (rule.namespace_id IS NULL OR namespace_id = rule.namespace_id)
           AND created_at < p_now - make_interval(days => ttl);
        GET DIAGNOSTICS n = ROW_COUNT;
        retention_count := retention_count + n;
    END LOOP;
    RETURN QUERY SELECT explicit_count, retention_count;
END
$$;

-- Active memories whose current version has no embedding yet.
CREATE OR REPLACE FUNCTION scos_worker_embedding_backlog()
RETURNS bigint
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT count(*)
      FROM memory_items mi
      JOIN memory_versions mv ON mv.id = mi.current_version_id
     WHERE mi.status = 'active'
       AND NOT EXISTS (SELECT 1 FROM memory_embeddings me WHERE me.memory_version_id = mv.id)
$$;

-- Tenants with orchestration history, for per-tenant policy evaluation.
CREATE OR REPLACE FUNCTION scos_worker_policy_tenants()
RETURNS SETOF uuid
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT DISTINCT tenant_id FROM orchestration_runs WHERE tenant_id IS NOT NULL ORDER BY tenant_id
$$;

REVOKE ALL ON FUNCTION scos_worker_claim_outbox_jobs(text, text[], integer, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION scos_worker_synthesis_namespaces(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION scos_worker_unembedded_versions(uuid, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION scos_worker_store_embedding(uuid, uuid, text, text, text, integer, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION scos_worker_expire_memories(timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION scos_worker_embedding_backlog() FROM PUBLIC;
REVOKE ALL ON FUNCTION scos_worker_policy_tenants() FROM PUBLIC;

COMMIT;
