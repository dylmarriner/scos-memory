BEGIN;

-- Durable governance planes. JSON columns carry extension metadata and payloads,
-- but lifecycle identity, status, ordering and ownership remain relational.
CREATE TABLE IF NOT EXISTS entities (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    entity_key text NOT NULL,
    entity_type text NOT NULL,
    display_name text NOT NULL,
    scope text NOT NULL DEFAULT 'tenant',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, entity_key)
);

CREATE TABLE IF NOT EXISTS entity_aliases (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    entity_id uuid NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    alias text NOT NULL,
    alias_type text NOT NULL DEFAULT 'alternate',
    confidence double precision NOT NULL DEFAULT 1 CHECK (confidence BETWEEN 0 AND 1),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, entity_id, alias)
);

CREATE TABLE IF NOT EXISTS entity_representations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    entity_id uuid NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    representation_type text NOT NULL,
    representation jsonb NOT NULL,
    source text NOT NULL DEFAULT 'observed',
    valid_from timestamptz,
    valid_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS beliefs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    premise text,
    conclusion text NOT NULL,
    confidence double precision NOT NULL CHECK (confidence BETWEEN 0 AND 1),
    status text NOT NULL DEFAULT 'active',
    revised_by uuid REFERENCES beliefs(id),
    derivation text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS belief_evidence (
    belief_id uuid NOT NULL REFERENCES beliefs(id) ON DELETE CASCADE,
    evidence_id uuid NOT NULL REFERENCES memory_evidence(id) ON DELETE RESTRICT,
    role text NOT NULL DEFAULT 'supports',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (belief_id, evidence_id)
);

CREATE TABLE IF NOT EXISTS diary_entries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    principal_id uuid REFERENCES principals(id) ON DELETE SET NULL,
    session_id uuid REFERENCES sessions(id) ON DELETE SET NULL,
    turn_id uuid REFERENCES turns(id) ON DELETE SET NULL,
    entry_type text NOT NULL DEFAULT 'observation',
    content text NOT NULL,
    source text NOT NULL DEFAULT 'system',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS plugin_packages (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    package_id text NOT NULL,
    version text NOT NULL,
    manifest jsonb NOT NULL DEFAULT '{}'::jsonb,
    content_hash text,
    signature text,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, package_id, version)
);

CREATE TABLE IF NOT EXISTS plugin_installations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    package_id uuid NOT NULL REFERENCES plugin_packages(id) ON DELETE CASCADE,
    status text NOT NULL DEFAULT 'installed',
    enabled boolean NOT NULL DEFAULT false,
    isolation text,
    installed_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, package_id)
);

CREATE TABLE IF NOT EXISTS skills (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    skill_id text NOT NULL,
    name text NOT NULL,
    description text,
    parameters jsonb NOT NULL DEFAULT '[]'::jsonb,
    status text NOT NULL DEFAULT 'enabled',
    version text,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, skill_id)
);

CREATE TABLE IF NOT EXISTS skill_executions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    skill_id uuid NOT NULL REFERENCES skills(id) ON DELETE RESTRICT,
    status text NOT NULL DEFAULT 'running',
    input jsonb NOT NULL DEFAULT '{}'::jsonb,
    output jsonb,
    error text,
    started_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

CREATE TABLE IF NOT EXISTS hooks (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    hook_id text NOT NULL,
    stage text NOT NULL,
    status text NOT NULL DEFAULT 'enabled',
    definition jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, hook_id)
);

CREATE TABLE IF NOT EXISTS hook_subscriptions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    hook_id uuid NOT NULL REFERENCES hooks(id) ON DELETE CASCADE,
    event_type text NOT NULL,
    priority integer NOT NULL DEFAULT 0,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, hook_id, event_type)
);

CREATE TABLE IF NOT EXISTS hook_executions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    hook_id uuid NOT NULL REFERENCES hooks(id) ON DELETE RESTRICT,
    subscription_id uuid REFERENCES hook_subscriptions(id) ON DELETE SET NULL,
    status text NOT NULL DEFAULT 'running',
    input jsonb NOT NULL DEFAULT '{}'::jsonb,
    output jsonb,
    error text,
    started_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

CREATE TABLE IF NOT EXISTS agent_adapters (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    adapter_id text NOT NULL,
    boundary text NOT NULL,
    gateway_url text,
    status text NOT NULL DEFAULT 'enabled',
    configuration jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, adapter_id)
);

CREATE TABLE IF NOT EXISTS adapter_instances (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    adapter_id uuid NOT NULL REFERENCES agent_adapters(id) ON DELETE CASCADE,
    agent_id uuid REFERENCES agents(id) ON DELETE SET NULL,
    instance_key text NOT NULL,
    status text NOT NULL DEFAULT 'ready',
    last_seen_at timestamptz,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (tenant_id, instance_key)
);

CREATE TABLE IF NOT EXISTS capability_graph (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    source_id text NOT NULL,
    target_id text NOT NULL,
    relation text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, source_id, target_id, relation)
);

CREATE TABLE IF NOT EXISTS transparency_projections (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    projection_name text NOT NULL,
    projection jsonb NOT NULL DEFAULT '{}'::jsonb,
    source_sequence bigint NOT NULL DEFAULT 0,
    generated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, projection_name)
);

CREATE TABLE IF NOT EXISTS dashboard_stream_cursors (
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    consumer_id text NOT NULL,
    stream_name text NOT NULL,
    last_event_id uuid,
    last_sequence bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, consumer_id, stream_name)
);

CREATE TABLE IF NOT EXISTS recovery_runs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id text NOT NULL,
    phase text NOT NULL,
    status text NOT NULL DEFAULT 'started',
    checklist jsonb NOT NULL DEFAULT '[]'::jsonb,
    interrupted_turn_count integer NOT NULL DEFAULT 0,
    open_operation_count integer NOT NULL DEFAULT 0,
    pending_outbox_count integer NOT NULL DEFAULT 0,
    replay_cursor bigint NOT NULL DEFAULT 0,
    recovery_started timestamptz NOT NULL DEFAULT now(),
    recovery_ended timestamptz,
    error_detail text
);

CREATE TABLE IF NOT EXISTS recovery_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    recovery_run_id uuid NOT NULL REFERENCES recovery_runs(id) ON DELETE CASCADE,
    sequence bigint NOT NULL,
    event_type text NOT NULL,
    subject_id text,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    replay_state text NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now(),
    replayed_at timestamptz,
    UNIQUE (recovery_run_id, sequence)
);

CREATE TABLE IF NOT EXISTS recovery_leases (
    lease_key text PRIMARY KEY,
    recovery_run_id uuid REFERENCES recovery_runs(id) ON DELETE SET NULL,
    owner text NOT NULL,
    state text NOT NULL DEFAULT 'active',
    acquired_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    released_at timestamptz
);

CREATE INDEX IF NOT EXISTS recovery_events_replay_idx ON recovery_events (recovery_run_id, replay_state, sequence);
CREATE INDEX IF NOT EXISTS recovery_runs_service_idx ON recovery_runs (service_id, recovery_started DESC);
CREATE INDEX IF NOT EXISTS diary_entries_timeline_idx ON diary_entries (tenant_id, occurred_at DESC);

COMMIT;
