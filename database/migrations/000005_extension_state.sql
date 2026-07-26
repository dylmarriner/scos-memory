BEGIN;

-- Extension metadata is authoritative operational state. It must survive a
-- manager restart and is stored as a single transactionally replaced JSON
-- document so package, skill, hook, and receipt updates are atomic together.
CREATE TABLE IF NOT EXISTS extension_manager_state (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    state jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

COMMIT;
