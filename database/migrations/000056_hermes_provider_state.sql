BEGIN;

-- hermes-provider state that was accepted and then thrown away.
--
-- sync_turn took a turn's tool calls and stored only its messages, so a
-- recalled turn never showed what the agent ran. The calls are kept on the
-- turn they belong to, as the JSON array the caller sent.
ALTER TABLE turns
    ADD COLUMN IF NOT EXISTS tool_calls jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE turns
    DROP CONSTRAINT IF EXISTS turns_tool_calls_is_array;
ALTER TABLE turns
    ADD CONSTRAINT turns_tool_calls_is_array CHECK (jsonb_typeof(tool_calls) = 'array');

-- save_config validated a Hermes setup and stored nothing; initialize echoed
-- its request. One row per principal and Hermes profile scope holds the saved
-- setup (never a secret: the provider rejects secret keys, and
-- optional_auth_reference only names a Hermes-managed secret) and the
-- profile the provider was last initialised against.
CREATE TABLE IF NOT EXISTS hermes_provider_configs (
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    profile_scope text NOT NULL DEFAULT '',
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    profile_path text,
    workspace text,
    initialized_at timestamptz,
    config_updated_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, principal_id, profile_scope),
    CHECK (jsonb_typeof(config) = 'object')
);

ALTER TABLE hermes_provider_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE hermes_provider_configs FORCE ROW LEVEL SECURITY;

-- A principal's own setup only: another principal in the same tenant can
-- neither read nor overwrite it.
DROP POLICY IF EXISTS hermes_provider_configs_owner_access ON hermes_provider_configs;
CREATE POLICY hermes_provider_configs_owner_access ON hermes_provider_configs
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
    );

COMMIT;
