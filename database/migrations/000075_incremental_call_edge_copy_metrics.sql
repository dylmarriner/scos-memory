BEGIN;
-- P3: distinguish graph rows copied from a compatible unchanged parent
-- from rows recomputed by the target's resolver. Both counters commit with
-- snapshot activation and roll back on failed indexing. Resolver-version
-- provenance prevents old semantic graph rules being silently inherited
-- when source extraction stays unchanged.
ALTER TABLE code_repository_snapshots
    ADD COLUMN IF NOT EXISTS copied_call_edge_count integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS resolved_call_edge_count integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS call_resolver_version text NOT NULL DEFAULT 'legacy';
COMMIT;
