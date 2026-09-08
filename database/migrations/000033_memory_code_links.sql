BEGIN;

-- P1-C: explicit memory<->code evidence links, seeding the §25 moat (a
-- fact and the code symbol version it evidences, was motivated by, or was
-- invalidated by). New table, so RLS is enabled+forced+policied in the
-- same migration that creates it -- P0-0 established this convention after
-- finding capabilities had RLS enabled but no policy for months.
CREATE TABLE memory_code_links (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    fact_id uuid NOT NULL REFERENCES memory_items(id) ON DELETE CASCADE,
    code_symbol_version_id uuid NOT NULL REFERENCES code_symbol_versions(id) ON DELETE CASCADE,
    link_type text NOT NULL CHECK (link_type IN ('evidences','motivated_by','invalidated_by')),
    confidence float NOT NULL DEFAULT 1.0,
    source text NOT NULL CHECK (source IN ('explicit','okf_promotion','inferred')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (fact_id, code_symbol_version_id, link_type)
);
CREATE INDEX memory_code_links_fact_idx ON memory_code_links (fact_id);
CREATE INDEX memory_code_links_symbol_version_idx ON memory_code_links (code_symbol_version_id);

ALTER TABLE memory_code_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_code_links FORCE ROW LEVEL SECURITY;

-- Same shape as memory_evidence_item_access (000003): access follows the
-- referenced memory_items row, whose own RLS policy already enforces
-- tenant/namespace membership -- this table adds no looser a boundary than
-- the fact it links already has.
CREATE POLICY memory_code_links_item_access ON memory_code_links
    FOR ALL USING (EXISTS (SELECT 1 FROM memory_items i WHERE i.id = memory_code_links.fact_id))
    WITH CHECK (EXISTS (SELECT 1 FROM memory_items i WHERE i.id = memory_code_links.fact_id)
        AND tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
