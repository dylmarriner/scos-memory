BEGIN;

-- credential-vault gives every tenant's agents a first-party, persistent,
-- encrypted store for secret material (API keys, passwords, tokens) that
-- lives inside the fabric rather than as a pointer to an external manager
-- (contrast security_gate.secret_references, which deliberately stores no
-- values -- see 000013_security_gate_persistence.sql). Values are encrypted
-- at rest with AES-256-GCM by the service before insertion; this schema
-- never sees plaintext.
--
-- Tenant isolation is enforced by row-level security, same as every other
-- governed table. Per-key access is a separate, finer-grained control:
-- credential_acl lists exactly which principals (including agents, which
-- are a principal subtype -- see agents(id) REFERENCES principals(id) in
-- 000001_initial.sql) may read or write a given credential. Tenant
-- membership alone does not grant access to a credential's value.

CREATE TABLE credentials (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    name text NOT NULL,
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL,
    key_version integer NOT NULL DEFAULT 1,
    created_by uuid NOT NULL REFERENCES principals(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE credential_acl (
    credential_id uuid NOT NULL REFERENCES credentials(id) ON DELETE CASCADE,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    principal_id uuid NOT NULL REFERENCES principals(id),
    permission text NOT NULL CHECK (permission IN ('read', 'write')),
    granted_by uuid NOT NULL REFERENCES principals(id),
    granted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (credential_id, principal_id, permission)
);

CREATE INDEX credential_acl_principal_idx ON credential_acl (tenant_id, principal_id);

ALTER TABLE credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE credential_acl ENABLE ROW LEVEL SECURITY;

CREATE POLICY credential_tenant_access ON credentials
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY credential_acl_tenant_access ON credential_acl
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
