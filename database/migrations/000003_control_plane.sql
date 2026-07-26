BEGIN;

CREATE TABLE capabilities (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    capability_id text NOT NULL,
    capability_type text NOT NULL,
    version text NOT NULL,
    supported_intents jsonb NOT NULL DEFAULT '[]'::jsonb,
    input_schema text,
    output_schema text,
    required_permissions jsonb NOT NULL DEFAULT '{}'::jsonb,
    maximum_data_classification sensitivity_class NOT NULL DEFAULT 'internal',
    read_or_write_class text NOT NULL,
    side_effect_class text NOT NULL,
    confirmation_policy text NOT NULL,
    idempotency_support boolean NOT NULL DEFAULT false,
    reconciliation_support boolean NOT NULL DEFAULT false,
    health_state text NOT NULL DEFAULT 'unknown',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    registered_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (capability_id, version)
);

CREATE TABLE lifecycle_stages (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    session_id uuid REFERENCES sessions(id),
    turn_id uuid REFERENCES turns(id),
    stage_name text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending','running','completed','failed','interrupted','skipped')),
    input_reference text,
    output_reference text,
    policy_version text,
    recovery_instruction text,
    started_at timestamptz,
    completed_at timestamptz,
    error_detail text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE routing_decisions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    session_id uuid REFERENCES sessions(id),
    turn_id uuid REFERENCES turns(id),
    actor_id uuid NOT NULL REFERENCES principals(id),
    intent_labels jsonb NOT NULL,
    resolved_scope jsonb NOT NULL,
    risk_class text NOT NULL,
    freshness_requirement text,
    candidate_capabilities jsonb NOT NULL,
    selected_plan jsonb NOT NULL,
    rejected_alternatives jsonb NOT NULL DEFAULT '[]'::jsonb,
    reason_codes jsonb NOT NULL,
    confidence double precision NOT NULL CHECK (confidence BETWEEN 0 AND 1),
    policy_version text NOT NULL,
    registry_generation text NOT NULL,
    estimated_tokens integer,
    estimated_cost numeric,
    actual_outcome jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE outbox_dead_letters (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id uuid NOT NULL REFERENCES outbox_jobs(id),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    payload jsonb NOT NULL,
    attempts integer NOT NULL,
    failure_reason text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE capabilities ENABLE ROW LEVEL SECURITY;
ALTER TABLE lifecycle_stages ENABLE ROW LEVEL SECURITY;
ALTER TABLE routing_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_dead_letters ENABLE ROW LEVEL SECURITY;

CREATE POLICY lifecycle_stage_tenant_access ON lifecycle_stages
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY routing_decision_tenant_access ON routing_decisions
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid AND actor_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid AND actor_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid);
CREATE POLICY dead_letter_tenant_access ON outbox_dead_letters
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
