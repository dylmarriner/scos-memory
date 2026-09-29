BEGIN;

-- Background workers (consolidation, embeddings, index-maintenance,
-- policy-evaluation) process outbox jobs and maintenance sweeps for every
-- tenant. Every policy defined so far keys off per-request context
-- (scos.tenant_id / scos.principal_id) that a background worker does not
-- have, so under FORCE ROW LEVEL SECURITY (000012) a scos_app connection
-- with no context sees zero rows: the workers claimed nothing and silently
-- did no work.
--
-- scos_worker is a separate login role with its own password, supplied by
-- the caller via psql -v worker_password=... (see scripts/scos-migrate),
-- required and never defaulted -- the same contract as scos_app in 000011.
-- It is NOBYPASSRLS: cross-tenant access comes from the role-scoped policies
-- below, so it works on managed PostgreSQL where BYPASSRLS roles cannot be
-- created, and holding the scos_app credential grants none of it. Table
-- privileges are the exact operations the workers issue, nothing broader.
SELECT format('ALTER ROLE scos_worker PASSWORD %L;', :'worker_password')
WHERE EXISTS (SELECT FROM pg_roles WHERE rolname = 'scos_worker')
UNION ALL
SELECT format('CREATE ROLE scos_worker LOGIN PASSWORD %L NOSUPERUSER NOBYPASSRLS;', :'worker_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'scos_worker')
\gexec

GRANT USAGE ON SCHEMA public TO scos_worker;

-- consolidation-worker: outbox claim/complete, turn context, candidate
-- extraction, promotion, dedup merge, supersession, synthesis.
GRANT SELECT, UPDATE ON outbox_jobs TO scos_worker;
GRANT SELECT ON events, turns, sessions, messages, namespaces TO scos_worker;
GRANT SELECT, INSERT, UPDATE ON memory_items, memory_candidates, memory_synthesis_state TO scos_worker;
GRANT SELECT, INSERT ON memory_versions, memory_evidence, memory_relations, memory_reviews TO scos_worker;
-- memory_items / memory_evidence inserts fire unified_graph_sync_node(),
-- which runs as the invoking role.
GRANT SELECT, INSERT ON unified_graph_nodes TO scos_worker;

-- embeddings-worker: embedding space registry and per-version vectors.
GRANT SELECT, INSERT, UPDATE ON embedding_spaces, memory_embeddings TO scos_worker;

-- index-maintenance-worker: retention sweeps over memory_items (granted
-- above) driven by retention_rules.
GRANT SELECT ON retention_rules TO scos_worker;

-- policy-evaluation-worker: shadow evaluation of routing policies.
GRANT SELECT ON orchestration_runs, routing_decisions TO scos_worker;
GRANT SELECT, INSERT ON audit_entries TO scos_worker;

-- index-maintenance-worker also runs VACUUM ANALYZE on the tables it prunes.
-- Non-owners need the MAINTAIN privilege, which exists from PostgreSQL 17;
-- on older servers the worker reports those tables as skipped and
-- autovacuum remains responsible for them.
DO $$
BEGIN
    IF current_setting('server_version_num')::int >= 170000 THEN
        EXECUTE 'GRANT MAINTAIN ON memory_embeddings, memory_versions, memory_items TO scos_worker';
    END IF;
END
$$;

-- Role-scoped policies: permissive policies are OR'd with the existing
-- tenant/principal policies, and apply only to connections authenticated as
-- scos_worker. The GRANTs above remain the gate on which commands run.
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'outbox_jobs', 'events', 'turns', 'sessions', 'messages', 'namespaces',
        'memory_items', 'memory_candidates', 'memory_versions', 'memory_evidence',
        'memory_relations', 'memory_reviews', 'unified_graph_nodes',
        'memory_embeddings', 'retention_rules',
        'orchestration_runs', 'routing_decisions', 'audit_entries'
    ] LOOP
        EXECUTE format('DROP POLICY IF EXISTS scos_worker_access ON %I', t);
        EXECUTE format(
            'CREATE POLICY scos_worker_access ON %I FOR ALL TO scos_worker USING (true) WITH CHECK (true)',
            t);
    END LOOP;
END
$$;

COMMIT;
