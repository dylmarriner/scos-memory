BEGIN;

-- P0-0: make RLS actually enforce, final step. Every table below already
-- has ENABLE ROW LEVEL SECURITY plus at least one CREATE POLICY somewhere
-- under database/policies/ or database/migrations/ (enumerated and
-- verified table-by-table while scoping this fix -- the one table that
-- had ENABLE without a policy, capabilities, was given one in
-- 000010_capabilities_rls.sql specifically so it could be included here
-- safely). FORCE ROW LEVEL SECURITY makes the policies bind even against
-- the table-owning role -- without it, RLS-enabled-but-not-forced tables
-- are still fully readable/writable by the owner (and, before this
-- migration series, that owner was the only role any service ever
-- connected as).
--
-- namespace_memberships is deliberately excluded: it carries no RLS by
-- design, since it is the ACL table nearly every other table's policy
-- subqueries to resolve tenant/namespace membership in the first place --
-- forcing RLS on it would require every other policy to itself be
-- rewritten to somehow read it first, which is circular. This matches the
-- comparative-divergence-audit's F-09 note; giving namespace_memberships
-- its own access model is a separate, real design question, out of scope
-- here.

ALTER TABLE turns FORCE ROW LEVEL SECURITY;
ALTER TABLE events FORCE ROW LEVEL SECURITY;
ALTER TABLE operations FORCE ROW LEVEL SECURITY;
ALTER TABLE outbox_jobs FORCE ROW LEVEL SECURITY;
ALTER TABLE orchestration_runs FORCE ROW LEVEL SECURITY;
ALTER TABLE routing_decisions FORCE ROW LEVEL SECURITY;
ALTER TABLE message_chunks FORCE ROW LEVEL SECURITY;
ALTER TABLE checkpoints FORCE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
ALTER TABLE memory_evidence FORCE ROW LEVEL SECURITY;
ALTER TABLE memory_relations FORCE ROW LEVEL SECURITY;
ALTER TABLE memory_embeddings FORCE ROW LEVEL SECURITY;
ALTER TABLE memory_candidates FORCE ROW LEVEL SECURITY;
ALTER TABLE memory_reviews FORCE ROW LEVEL SECURITY;
ALTER TABLE object_references FORCE ROW LEVEL SECURITY;
ALTER TABLE retention_rules FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_entries FORCE ROW LEVEL SECURITY;
ALTER TABLE service_state FORCE ROW LEVEL SECURITY;
ALTER TABLE extension_manager_state FORCE ROW LEVEL SECURITY;
ALTER TABLE entities FORCE ROW LEVEL SECURITY;
ALTER TABLE entity_aliases FORCE ROW LEVEL SECURITY;
ALTER TABLE entity_representations FORCE ROW LEVEL SECURITY;
ALTER TABLE beliefs FORCE ROW LEVEL SECURITY;
ALTER TABLE belief_evidence FORCE ROW LEVEL SECURITY;
ALTER TABLE diary_entries FORCE ROW LEVEL SECURITY;
ALTER TABLE plugin_packages FORCE ROW LEVEL SECURITY;
ALTER TABLE plugin_installations FORCE ROW LEVEL SECURITY;
ALTER TABLE skills FORCE ROW LEVEL SECURITY;
ALTER TABLE skill_executions FORCE ROW LEVEL SECURITY;
ALTER TABLE hooks FORCE ROW LEVEL SECURITY;
ALTER TABLE hook_subscriptions FORCE ROW LEVEL SECURITY;
ALTER TABLE hook_executions FORCE ROW LEVEL SECURITY;
ALTER TABLE agent_adapters FORCE ROW LEVEL SECURITY;
ALTER TABLE adapter_instances FORCE ROW LEVEL SECURITY;
ALTER TABLE capability_graph FORCE ROW LEVEL SECURITY;
ALTER TABLE transparency_projections FORCE ROW LEVEL SECURITY;
ALTER TABLE dashboard_stream_cursors FORCE ROW LEVEL SECURITY;
ALTER TABLE recovery_runs FORCE ROW LEVEL SECURITY;
ALTER TABLE recovery_events FORCE ROW LEVEL SECURITY;
ALTER TABLE recovery_leases FORCE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
ALTER TABLE principals FORCE ROW LEVEL SECURITY;
ALTER TABLE agents FORCE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
ALTER TABLE workspaces FORCE ROW LEVEL SECURITY;
ALTER TABLE agent_registrations FORCE ROW LEVEL SECURITY;
ALTER TABLE agent_handoffs FORCE ROW LEVEL SECURITY;
ALTER TABLE tool_definitions FORCE ROW LEVEL SECURITY;
ALTER TABLE context_envelopes FORCE ROW LEVEL SECURITY;
ALTER TABLE security_tokens FORCE ROW LEVEL SECURITY;
ALTER TABLE secret_references FORCE ROW LEVEL SECURITY;
ALTER TABLE hierarchy_nodes FORCE ROW LEVEL SECURITY;
ALTER TABLE namespaces FORCE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
ALTER TABLE messages FORCE ROW LEVEL SECURITY;
ALTER TABLE memory_items FORCE ROW LEVEL SECURITY;
ALTER TABLE memory_versions FORCE ROW LEVEL SECURITY;
ALTER TABLE tool_executions FORCE ROW LEVEL SECURITY;
ALTER TABLE okf_bundles FORCE ROW LEVEL SECURITY;
ALTER TABLE okf_concepts FORCE ROW LEVEL SECURITY;
ALTER TABLE retrieval_runs FORCE ROW LEVEL SECURITY;
ALTER TABLE retrieval_results FORCE ROW LEVEL SECURITY;
ALTER TABLE capabilities FORCE ROW LEVEL SECURITY;
ALTER TABLE lifecycle_stages FORCE ROW LEVEL SECURITY;
ALTER TABLE outbox_dead_letters FORCE ROW LEVEL SECURITY;
ALTER TABLE okf_proposals FORCE ROW LEVEL SECURITY;
ALTER TABLE okf_proposal_transitions FORCE ROW LEVEL SECURITY;
ALTER TABLE okf_governance_roles FORCE ROW LEVEL SECURITY;
ALTER TABLE credentials FORCE ROW LEVEL SECURITY;
ALTER TABLE credential_acl FORCE ROW LEVEL SECURITY;

COMMIT;
