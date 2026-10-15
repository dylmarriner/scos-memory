BEGIN;

-- Tool governance used to register and authorise any tool name it was
-- handed by /v1/operations/prepare, and the decoder silently dropped the
-- plan and risk the caller sent with it. Prepare now refuses an unknown
-- tool: a tenant's tools are registered through the admin-scoped
-- /v1/tools/register path and authorised through /v1/tools/transition.
--
-- The fabric's own built-in tools are shipped here as data instead. The
-- catalog is global reference data, readable by any authenticated service
-- principal and writable by nobody at runtime: there is no INSERT, UPDATE
-- or DELETE policy, and RLS is forced so the restricted scos_app role is
-- bound by that. A built-in tool is materialised into a tenant's
-- tool_definitions (state 'authorised', properties copied from this row)
-- the first time that tenant prepares an operation for it, so an operator
-- can still cancel it per tenant through the normal lifecycle.
CREATE TABLE IF NOT EXISTS builtin_tool_definitions (
    name text PRIMARY KEY,
    description text NOT NULL,
    version text NOT NULL,
    properties jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- execution-proxy runs arbitrary planned shell commands: it can write and
-- destroy, its effects leave the fabric, re-running it is not safe, and it
-- supports dry runs and reconciliation of uncertain outcomes. Whether a
-- particular command needs confirmation is decided per operation from the
-- plan's risk level, so the tool itself is not confirmation-gated.
INSERT INTO builtin_tool_definitions (name, description, version, properties) VALUES
    ('execution-proxy',
     'Adaptive execution proxy: runs a planned shell command under tool governance and preserves its raw result',
     '0.1.0',
     '{"read_only": false, "destructive": true, "idempotent": false, "externally_side_effecting": true, "confirmation_gated": false, "dry_run_capable": true, "reconcilable": true}'::jsonb)
ON CONFLICT (name) DO NOTHING;

ALTER TABLE builtin_tool_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE builtin_tool_definitions FORCE ROW LEVEL SECURITY;
CREATE POLICY builtin_tool_definitions_authenticated_read ON builtin_tool_definitions
    FOR SELECT USING (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL);

DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'scos_app') THEN
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON builtin_tool_definitions FROM scos_app;
    END IF;
END
$$;

-- The governed plan, the risk the caller declared, the risk governance
-- decided on (the higher of the declared risk and the shared command risk
-- classifier's grade of the planned command) and whether the caller
-- confirmed the operation are now persisted with the operation's
-- tool_executions snapshot, beside the declared tool properties the
-- decision was checked against. Rows written before this migration have
-- no plan or risk, which the NULL risk columns record honestly.
ALTER TABLE tool_executions
    ADD COLUMN IF NOT EXISTS plan jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS requested_risk_level text,
    ADD COLUMN IF NOT EXISTS risk_level text,
    ADD COLUMN IF NOT EXISTS confirmed boolean NOT NULL DEFAULT false;

ALTER TABLE tool_executions
    DROP CONSTRAINT IF EXISTS tool_executions_risk_level_check,
    ADD CONSTRAINT tool_executions_risk_level_check
        CHECK (risk_level IS NULL OR risk_level IN ('read', 'test', 'write', 'destructive')),
    DROP CONSTRAINT IF EXISTS tool_executions_requested_risk_level_check,
    ADD CONSTRAINT tool_executions_requested_risk_level_check
        CHECK (requested_risk_level IS NULL OR requested_risk_level IN ('read', 'test', 'write', 'destructive'));

COMMIT;
