BEGIN;

-- P1-B: standing-answer cache. retrieval-orchestrator's handleRetrieve
-- checks this table before the 5-source fan-out (exact/FTS/vector/
-- relation/session); a hit skips straight to answering. question_key is a
-- normalized (lowercased, trimmed) exact match on the query text -- no
-- classifier, a repeated near-identical question is what gets cached.
-- evidence_fact_ids stored as JSON-encoded text, not a native uuid[]: a
-- native Postgres array requires a Scanner the stdlib database/sql driver
-- doesn't provide for free (see P1-D's called_names, same lesson).
CREATE TABLE memory_standing_answers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    namespace_id uuid NOT NULL,
    question_key text NOT NULL,
    answer text NOT NULL,
    evidence_fact_ids text NOT NULL DEFAULT '',
    confidence float NOT NULL DEFAULT 0.5,
    last_refreshed_at timestamptz NOT NULL DEFAULT now(),
    refresh_reason text,
    UNIQUE (tenant_id, namespace_id, question_key)
);
CREATE INDEX memory_standing_answers_lookup_idx ON memory_standing_answers (tenant_id, namespace_id, question_key);

ALTER TABLE memory_standing_answers ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_standing_answers FORCE ROW LEVEL SECURITY;

-- Same tenant+namespace-membership pattern as memory_items_namespace_access
-- (000001_rls.sql) -- P0-0's now-established convention of giving a new
-- table RLS + policy in the same migration that creates it.
CREATE POLICY memory_standing_answers_namespace_access ON memory_standing_answers
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships m
                    WHERE m.namespace_id = memory_standing_answers.namespace_id
                      AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
                      AND m.can_read))
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships m
                    WHERE m.namespace_id = memory_standing_answers.namespace_id
                      AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
                      AND m.can_write_candidates));

COMMIT;
