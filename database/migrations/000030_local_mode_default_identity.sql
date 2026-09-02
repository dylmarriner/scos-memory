-- Reserved tenant/principal/workspace/namespace for SCOS_AUTH_MODE=local
-- (self-hosted, single-user, no bearer token). mcp-gateway's resolveIdentity
-- resolves every caller to this fixed identity in that mode (see
-- services/mcp-gateway/cmd/server/main.go, localModeTenantID/
-- localModePrincipalID) so that downstream RLS policies -- which deny all
-- access when scos.tenant_id is unset rather than allowing it -- keep
-- working unchanged. Inert on hosted deployments; these rows are simply
-- never referenced unless a caller resolves to this UUID, which only
-- happens when the gateway itself is running in local mode.
--
-- Idempotent: safe to re-run, and safe to have already run before a
-- deployment ever turns local mode on.

INSERT INTO tenants (id, name)
VALUES ('00000000-0000-0000-0000-000000000001', 'Local (self-hosted, no auth)')
ON CONFLICT (id) DO NOTHING;

INSERT INTO principals (id, tenant_id, kind, external_id, display_name)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001',
    'service',
    'local-mode',
    'Local mode (no auth)'
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO workspaces (id, tenant_id, name)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001',
    'Local workspace'
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO namespaces (id, tenant_id, owner_principal_id, workspace_id, canonical_name)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001',
    'default'
)
ON CONFLICT (tenant_id, canonical_name) DO NOTHING;

INSERT INTO namespace_memberships (namespace_id, principal_id, can_read, can_write_candidates, can_promote, can_administer)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001',
    true, true, true, true
)
ON CONFLICT (namespace_id, principal_id) DO NOTHING;
