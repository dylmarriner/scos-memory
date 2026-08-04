BEGIN;

-- §26: durable proposal state and immutable transition evidence.  OKF is
-- platform governance content, so this table is principal-gated rather than
-- tenant-scoped like ordinary memory data.
CREATE TABLE okf_proposals (
    id text PRIMARY KEY,
    path text NOT NULL,
    base_commit text NOT NULL,
    base_hash text NOT NULL,
    content text NOT NULL,
    diff text NOT NULL,
    status text NOT NULL CHECK (status IN ('proposed','validated','diffed','under_review','approved','rejected','merged','indexed')),
    proposer text NOT NULL,
    reviewer text,
    review_role text,
    decision text,
    reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    reviewed_at timestamptz,
    merged_commit text,
    indexed_at timestamptz,
    diverged boolean NOT NULL DEFAULT false
);
CREATE INDEX okf_proposals_status_idx ON okf_proposals(status);
CREATE INDEX okf_proposals_path_idx ON okf_proposals(path);

CREATE TABLE okf_proposal_transitions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    proposal_id text NOT NULL REFERENCES okf_proposals(id),
    from_status text,
    to_status text NOT NULL,
    actor text NOT NULL,
    governance_role text,
    reason text NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE okf_governance_roles (
    principal_id text NOT NULL,
    role text NOT NULL CHECK (role IN ('system_owner','memory_steward','security_steward','knowledge_curator','agent_maintainer','autonomy_steward','operator','auditor')),
    active boolean NOT NULL DEFAULT true,
    assigned_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, role)
);

ALTER TABLE okf_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE okf_proposal_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE okf_governance_roles ENABLE ROW LEVEL SECURITY;
CREATE POLICY okf_proposals_authenticated_access ON okf_proposals FOR ALL USING (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL) WITH CHECK (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL);
CREATE POLICY okf_proposal_transitions_authenticated_access ON okf_proposal_transitions FOR ALL USING (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL) WITH CHECK (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL);
CREATE POLICY okf_governance_roles_authenticated_access ON okf_governance_roles FOR SELECT USING (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL);
REVOKE UPDATE, DELETE ON okf_proposal_transitions FROM PUBLIC;
CREATE TRIGGER okf_proposal_transitions_append_only BEFORE UPDATE OR DELETE ON okf_proposal_transitions FOR EACH ROW EXECUTE FUNCTION reject_authoritative_history_mutation();

COMMIT;
