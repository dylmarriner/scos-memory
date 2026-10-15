BEGIN;

-- The local-mode user (000030) owns the whole self-hosted installation, but
-- permission checks are now decided against grants the principal holds
-- (000048), and nothing ever issued one to it: every check it made was
-- denied. Grant it every action on every resource up to restricted. Secret
-- material still takes an explicit secret grant and a secret-class token.
-- Inert on hosted deployments, where no caller resolves to this principal.
INSERT INTO security_permission_grants (tenant_id, principal_id, action, resource_pattern, max_data_class)
SELECT '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001', '*', '*', 'restricted'
WHERE EXISTS (SELECT 1 FROM principals WHERE id = '00000000-0000-0000-0000-000000000001')
  AND NOT EXISTS (
      SELECT 1 FROM security_permission_grants
       WHERE tenant_id = '00000000-0000-0000-0000-000000000001'
         AND principal_id = '00000000-0000-0000-0000-000000000001'
         AND action = '*' AND resource_pattern = '*' AND revoked_at IS NULL);

COMMIT;
