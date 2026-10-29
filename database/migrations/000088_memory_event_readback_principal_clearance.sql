BEGIN;

-- P5 read-time security: membership alone does not prove that an authenticated
-- principal is enabled, belongs to the tenant, or has agent clearance.
-- Preserve current-memory visibility and event-namespace sensitivity rules.
DROP POLICY memory_system_events_read ON memory_system_events;
CREATE POLICY memory_system_events_read ON memory_system_events FOR SELECT USING (
    tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
    AND EXISTS (
        SELECT 1 FROM memory_items i
        WHERE i.id = memory_system_events.memory_id
          AND i.tenant_id = memory_system_events.tenant_id
    )
    AND EXISTS (
        SELECT 1 FROM principals p
        JOIN namespace_memberships m ON m.principal_id = p.id
        JOIN namespaces n ON n.id = m.namespace_id
        WHERE p.id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
          AND p.tenant_id = memory_system_events.tenant_id
          AND p.disabled_at IS NULL
          AND n.id = memory_system_events.namespace_id
          AND n.tenant_id = memory_system_events.tenant_id
          AND (m.can_read OR m.can_administer)
          AND memory_system_events.sensitivity <= n.sensitivity_ceiling
          AND (
              p.kind <> 'agent'
              OR EXISTS (
                  SELECT 1 FROM agents a
                  WHERE a.id = p.id
                    AND memory_system_events.sensitivity <= a.maximum_sensitivity
              )
          )
    )
);

COMMIT;
