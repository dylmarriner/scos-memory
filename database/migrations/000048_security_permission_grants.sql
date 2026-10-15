BEGIN;

-- security-gate's permission check previously allowed every non-secret
-- data class regardless of action, resource, delegation or expiry, and let
-- unknown classes through. Checks are now decided against durable grants
-- the principal actually holds, and delegated checks against a bounded,
-- expiring delegation plus the delegator's own grants.
--
-- Patterns (action, resource_pattern) are an exact value or a prefix
-- ending in a single trailing '*'; '*' alone matches everything. The
-- CHECK constraints mirror validPattern in
-- services/security-gate/cmd/server/permission_policy.go so a row written
-- outside the service still cannot carry an ambiguous pattern or an
-- unknown data class.

CREATE TABLE IF NOT EXISTS security_permission_grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    principal_id uuid NOT NULL REFERENCES principals(id),
    action text NOT NULL CHECK (action <> '' AND position('*' IN action) IN (0, length(action))),
    resource_pattern text NOT NULL CHECK (resource_pattern <> '' AND position('*' IN resource_pattern) IN (0, length(resource_pattern))),
    max_data_class text NOT NULL CHECK (max_data_class IN ('public', 'internal', 'confidential', 'restricted', 'secret')),
    granted_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    revoked_at timestamptz
);

CREATE INDEX IF NOT EXISTS security_permission_grants_principal_idx
    ON security_permission_grants (tenant_id, principal_id) WHERE revoked_at IS NULL;

-- A delegation always expires and can never name its own delegator.
CREATE TABLE IF NOT EXISTS security_permission_delegations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    delegator_id uuid NOT NULL REFERENCES principals(id),
    delegate_id uuid NOT NULL REFERENCES principals(id),
    action text NOT NULL CHECK (action <> '' AND position('*' IN action) IN (0, length(action))),
    resource_pattern text NOT NULL CHECK (resource_pattern <> '' AND position('*' IN resource_pattern) IN (0, length(resource_pattern))),
    max_data_class text NOT NULL CHECK (max_data_class IN ('public', 'internal', 'confidential', 'restricted', 'secret')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CHECK (delegator_id <> delegate_id),
    CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS security_permission_delegations_delegate_idx
    ON security_permission_delegations (tenant_id, delegate_id) WHERE revoked_at IS NULL;

-- RLS + policy in the same migration that creates the table, matching the
-- convention established by 000035/000036 and the tenant policy on
-- security_tokens (policies/000008_security_gate_rls.sql).
ALTER TABLE security_permission_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE security_permission_grants FORCE ROW LEVEL SECURITY;
ALTER TABLE security_permission_delegations ENABLE ROW LEVEL SECURITY;
ALTER TABLE security_permission_delegations FORCE ROW LEVEL SECURITY;

CREATE POLICY security_permission_grants_tenant_access ON security_permission_grants
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

CREATE POLICY security_permission_delegations_tenant_access ON security_permission_delegations
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
