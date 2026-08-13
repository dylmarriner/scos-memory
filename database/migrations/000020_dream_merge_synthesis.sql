BEGIN;

-- Dream-style Merge (§16/§25): a duplicate/near-duplicate fact is folded
-- into a single canonical memory rather than kept as a second copy. The
-- barer fact is marked 'merged' (memory_items.superseded_by_id already
-- points at the canonical replacement, reused here exactly as it is for
-- 'superseded' -- the two statuses distinguish *why* a fact stopped being
-- current: contradicted vs. duplicated) and hidden from default reads,
-- surfaced only with include_merged=true.
ALTER TYPE memory_status ADD VALUE IF NOT EXISTS 'merged';

COMMIT;

BEGIN;

-- Dream-style Synthesis (§16/§25): tracks, per namespace, when it was last
-- considered for background pattern distillation, so a namespace is
-- reprocessed at most once per configured cadence rather than on every
-- consolidation-worker cycle.
CREATE TABLE IF NOT EXISTS memory_synthesis_state (
    namespace_id      uuid PRIMARY KEY REFERENCES namespaces(id) ON DELETE CASCADE,
    last_run_at       timestamptz,
    last_pattern_count integer NOT NULL DEFAULT 0,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

COMMIT;
