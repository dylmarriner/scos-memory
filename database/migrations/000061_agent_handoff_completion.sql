BEGIN;

-- A handoff's lifecycle allowed 'completed' but nothing reached it: once
-- accepted, a handoff stayed accepted forever. The receiving agent (or the
-- principal that registered it) now completes an accepted handoff, and the
-- completion -- who, when, and what was delivered -- is recorded alongside
-- the answer that accepted it.
ALTER TABLE agent_handoffs
    ADD COLUMN IF NOT EXISTS completed_by_principal text,
    ADD COLUMN IF NOT EXISTS completed_by_agent text,
    ADD COLUMN IF NOT EXISTS completed_at timestamptz,
    ADD COLUMN IF NOT EXISTS completion_summary text;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'agent_handoffs_completion_recorded') THEN
        ALTER TABLE agent_handoffs
            ADD CONSTRAINT agent_handoffs_completion_recorded
            CHECK (status <> 'completed' OR (completed_by_principal IS NOT NULL AND completed_at IS NOT NULL
                                             AND responded_at IS NOT NULL)) NOT VALID;
    END IF;
END
$$;

COMMIT;
