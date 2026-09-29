BEGIN;

-- §24.2 save_config / §24.4 minimal setup: the Hermes provider's validated
-- configuration, one row per principal and Hermes profile scope. Secrets are
-- never stored here -- optional_auth_reference names a Hermes-managed secret
-- -- and the provider rejects secret keys before writing.
CREATE TABLE IF NOT EXISTS hermes_provider_configs (
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    profile_scope text NOT NULL DEFAULT '',
    config jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, principal_id, profile_scope)
);

ALTER TABLE hermes_provider_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE hermes_provider_configs FORCE ROW LEVEL SECURITY;

-- A principal reads and writes only its own provider configuration.
CREATE POLICY hermes_provider_configs_owner ON hermes_provider_configs
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid);

COMMIT;
