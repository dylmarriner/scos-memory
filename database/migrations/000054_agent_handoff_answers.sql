BEGIN;

-- Agent handoffs used to be answered by anyone in the tenant: every response
-- other than the literal "accepted" was coerced to "rejected", expires_at was
-- stored but never applied, and the receiving agent's declared tool and
-- memory permissions were never consulted. The agent registry now records
-- who answered and when, refuses an expired handoff (and records it as
-- expired), lets only the addressed agent or the principal that registered
-- it answer, and checks what a handoff asks for against what the receiving
-- agent declares.

-- The principal that registered an agent operates it, and is the principal
-- allowed to answer handoffs addressed to it. Rows registered before this
-- migration have no recorded principal and can be answered only by a token
-- carrying the agent's own id.
ALTER TABLE agent_registrations
    ADD COLUMN IF NOT EXISTS principal_id text;

ALTER TABLE agent_handoffs
    ADD COLUMN IF NOT EXISTS required_tool_permissions jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS initiated_by_principal text,
    ADD COLUMN IF NOT EXISTS responded_by_principal text,
    ADD COLUMN IF NOT EXISTS responded_by_agent text,
    ADD COLUMN IF NOT EXISTS responded_at timestamptz,
    ADD COLUMN IF NOT EXISTS response_reason text;

-- NOT VALID: rows answered before this migration carry no responder. Every
-- write from here on is checked.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'agent_handoffs_status_check') THEN
        ALTER TABLE agent_handoffs
            ADD CONSTRAINT agent_handoffs_status_check
            CHECK (status IN ('pending', 'accepted', 'rejected', 'completed', 'expired')) NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'agent_handoffs_answer_recorded') THEN
        ALTER TABLE agent_handoffs
            ADD CONSTRAINT agent_handoffs_answer_recorded
            CHECK (status NOT IN ('accepted', 'rejected') OR (responded_by_principal IS NOT NULL AND responded_at IS NOT NULL)) NOT VALID;
    END IF;
END
$$;

COMMIT;
