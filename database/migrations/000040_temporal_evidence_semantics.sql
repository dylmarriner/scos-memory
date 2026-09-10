BEGIN;

-- valid_until is world time. This is system time: when SCOS stopped treating
-- a proposition as current after a supersession or merge.
ALTER TABLE memory_items ADD COLUMN belief_expired_at timestamptz;
CREATE INDEX memory_items_bitemporal_lookup_idx
    ON memory_items (tenant_id, created_at, valid_from, valid_until, belief_expired_at);

COMMIT;
