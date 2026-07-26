BEGIN;

CREATE TABLE IF NOT EXISTS capability_registry (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    capability_key text NOT NULL,
    version text NOT NULL,
    capability_type text NOT NULL,
    supported_intents text[] NOT NULL DEFAULT '{}',
    input_schema jsonb NOT NULL DEFAULT '{}'::jsonb,
    output_schema jsonb NOT NULL DEFAULT '{}'::jsonb,
    required_permissions text[] NOT NULL DEFAULT '{}',
    maximum_data_classification text NOT NULL DEFAULT 'internal',
    side_effect_class text NOT NULL DEFAULT 'read_only',
    confirmation_policy text NOT NULL DEFAULT 'none',
    idempotency_support boolean NOT NULL DEFAULT false,
    reconciliation_support boolean NOT NULL DEFAULT false,
    data_locality text NOT NULL DEFAULT 'local',
    health_state text NOT NULL DEFAULT 'unknown',
    quality_score double precision,
    historical_success_rate double precision,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    generation bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (capability_key, version)
);

CREATE TABLE IF NOT EXISTS orchestration_runs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid REFERENCES tenants(id) ON DELETE CASCADE,
    session_id uuid REFERENCES sessions(id) ON DELETE SET NULL,
    turn_id uuid REFERENCES turns(id) ON DELETE SET NULL,
    interaction_id text NOT NULL,
    status text NOT NULL,
    current_stage text NOT NULL,
    policy_version text NOT NULL,
    registry_generation bigint NOT NULL,
    checkpoint jsonb NOT NULL DEFAULT '{}'::jsonb,
    started_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    UNIQUE (tenant_id, interaction_id)
);

CREATE TABLE IF NOT EXISTS routing_decisions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    orchestration_run_id uuid NOT NULL REFERENCES orchestration_runs(id) ON DELETE CASCADE,
    decision_id text NOT NULL UNIQUE,
    intent_labels text[] NOT NULL,
    risk_class text NOT NULL,
    freshness_requirement text NOT NULL DEFAULT 'none',
    selected_plan jsonb NOT NULL,
    rejected_alternatives jsonb NOT NULL DEFAULT '[]'::jsonb,
    reason_codes text[] NOT NULL,
    confidence double precision NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    confirmation_required boolean NOT NULL DEFAULT false,
    context_capsule_id text,
    outcome jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS autonomous_triggers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    trigger_key text NOT NULL UNIQUE,
    event_type text NOT NULL,
    handler_capability_key text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    policy jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 000003 may already have created routing_decisions.  Reconcile that
-- tenant-scoped table before creating indexes or writing autonomous routes.
ALTER TABLE routing_decisions
    ADD COLUMN IF NOT EXISTS orchestration_run_id uuid REFERENCES orchestration_runs(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS decision_id text;

CREATE UNIQUE INDEX IF NOT EXISTS routing_decisions_decision_id_idx
    ON routing_decisions (decision_id)
    WHERE decision_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS capability_registry_intents_idx
    ON capability_registry USING gin (supported_intents);
CREATE INDEX IF NOT EXISTS orchestration_runs_status_idx
    ON orchestration_runs (status, current_stage);
CREATE INDEX IF NOT EXISTS routing_decisions_run_idx
    ON routing_decisions (orchestration_run_id, created_at);
CREATE INDEX IF NOT EXISTS autonomous_triggers_event_idx
    ON autonomous_triggers (event_type) WHERE enabled;

COMMIT;
