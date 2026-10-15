BEGIN;

-- security-gate's POST /v1/secrets/{id}/resolve returns a live secret value
-- but used to ignore both who the secret was meant for and how sensitive it
-- is: any authenticated principal in the tenant could resolve any
-- reference. A reference now carries the sensitivity class its value
-- belongs to, and resolve refuses a caller whose signed claims do not grant
-- clearance for that class (see services/security-gate secretAccess).
--
-- Existing rows default to 'secret' -- the most restrictive class -- so no
-- reference becomes resolvable by more callers than before this migration;
-- an owner who wants a lower class must set it explicitly.
--
-- secret_references keeps its tenant RLS policy
-- (database/policies/000008_security_gate_rls.sql) and FORCE ROW LEVEL
-- SECURITY (database/policies/000012_force_rls.sql); adding a column does
-- not change either, and no new table is introduced here.

ALTER TABLE secret_references
    ADD COLUMN IF NOT EXISTS data_class sensitivity_class NOT NULL DEFAULT 'secret';

COMMIT;
