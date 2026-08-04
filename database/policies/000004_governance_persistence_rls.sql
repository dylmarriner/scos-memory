BEGIN;

ALTER TABLE entities ENABLE ROW LEVEL SECURITY;
ALTER TABLE entity_aliases ENABLE ROW LEVEL SECURITY;
ALTER TABLE entity_representations ENABLE ROW LEVEL SECURITY;
ALTER TABLE beliefs ENABLE ROW LEVEL SECURITY;
ALTER TABLE belief_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE diary_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE plugin_packages ENABLE ROW LEVEL SECURITY;
ALTER TABLE plugin_installations ENABLE ROW LEVEL SECURITY;
ALTER TABLE skills ENABLE ROW LEVEL SECURITY;
ALTER TABLE skill_executions ENABLE ROW LEVEL SECURITY;
ALTER TABLE hooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE hook_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE hook_executions ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_adapters ENABLE ROW LEVEL SECURITY;
ALTER TABLE adapter_instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE capability_graph ENABLE ROW LEVEL SECURITY;
ALTER TABLE transparency_projections ENABLE ROW LEVEL SECURITY;
ALTER TABLE dashboard_stream_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE recovery_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE recovery_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE recovery_leases ENABLE ROW LEVEL SECURITY;

CREATE POLICY governance_tenant_access ON entities FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_alias_access ON entity_aliases FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_representation_access ON entity_representations FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_belief_access ON beliefs FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_belief_evidence_access ON belief_evidence FOR ALL USING (EXISTS (SELECT 1 FROM beliefs b WHERE b.id = belief_evidence.belief_id AND b.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)) WITH CHECK (EXISTS (SELECT 1 FROM beliefs b WHERE b.id = belief_evidence.belief_id AND b.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid));
CREATE POLICY governance_diary_access ON diary_entries FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_plugin_access ON plugin_packages FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_installation_access ON plugin_installations FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_skill_access ON skills FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_skill_execution_access ON skill_executions FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_hook_access ON hooks FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_hook_subscription_access ON hook_subscriptions FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_hook_execution_access ON hook_executions FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_adapter_access ON agent_adapters FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_instance_access ON adapter_instances FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_graph_access ON capability_graph FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_projection_access ON transparency_projections FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY governance_cursor_access ON dashboard_stream_cursors FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

CREATE POLICY recovery_service_access ON recovery_runs FOR ALL USING (service_id = NULLIF(current_setting('scos.service_id', true), '')) WITH CHECK (service_id = NULLIF(current_setting('scos.service_id', true), ''));
CREATE POLICY recovery_event_access ON recovery_events FOR ALL USING (EXISTS (SELECT 1 FROM recovery_runs r WHERE r.id = recovery_events.recovery_run_id AND r.service_id = NULLIF(current_setting('scos.service_id', true), ''))) WITH CHECK (EXISTS (SELECT 1 FROM recovery_runs r WHERE r.id = recovery_events.recovery_run_id AND r.service_id = NULLIF(current_setting('scos.service_id', true), '')));
CREATE POLICY recovery_lease_access ON recovery_leases FOR ALL USING (owner = NULLIF(current_setting('scos.service_id', true), '') OR EXISTS (SELECT 1 FROM recovery_runs r WHERE r.id = recovery_leases.recovery_run_id AND r.service_id = NULLIF(current_setting('scos.service_id', true), ''))) WITH CHECK (owner = NULLIF(current_setting('scos.service_id', true), '') OR EXISTS (SELECT 1 FROM recovery_runs r WHERE r.id = recovery_leases.recovery_run_id AND r.service_id = NULLIF(current_setting('scos.service_id', true), '')));

COMMIT;
