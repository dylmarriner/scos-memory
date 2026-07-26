BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TYPE principal_kind AS ENUM ('human', 'agent', 'service');
CREATE TYPE turn_status AS ENUM ('accepted', 'running', 'interrupted', 'completed', 'failed', 'cancelled');
CREATE TYPE operation_status AS ENUM ('proposed', 'prepared', 'authorised', 'executing', 'succeeded', 'failed', 'cancelled', 'unknown', 'reconciling', 'reconciled');
CREATE TYPE memory_type AS ENUM ('working', 'episodic', 'semantic', 'procedural', 'governing', 'artifact_reference', 'revocation');
CREATE TYPE memory_status AS ENUM ('candidate', 'active', 'quarantined', 'superseded', 'revoked', 'expired', 'deleted');
CREATE TYPE sensitivity_class AS ENUM ('public', 'internal', 'confidential', 'restricted', 'secret');
CREATE TYPE job_status AS ENUM ('pending', 'processing', 'succeeded', 'retryable', 'dead_letter');

CREATE TABLE tenants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE principals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    kind principal_kind NOT NULL,
    external_id text NOT NULL,
    display_name text NOT NULL,
    disabled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, external_id)
);

CREATE TABLE agents (
    id uuid PRIMARY KEY REFERENCES principals(id),
    version text NOT NULL,
    capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    maximum_sensitivity sensitivity_class NOT NULL DEFAULT 'internal',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE projects (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    name text NOT NULL,
    slug text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, slug)
);

CREATE TABLE workspaces (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    project_id uuid REFERENCES projects(id),
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE namespaces (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    owner_principal_id uuid REFERENCES principals(id),
    project_id uuid REFERENCES projects(id),
    workspace_id uuid REFERENCES workspaces(id),
    canonical_name text NOT NULL,
    sensitivity_ceiling sensitivity_class NOT NULL DEFAULT 'internal',
    retention_policy jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, canonical_name)
);

CREATE TABLE namespace_memberships (
    namespace_id uuid NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    can_read boolean NOT NULL DEFAULT false,
    can_write_candidates boolean NOT NULL DEFAULT false,
    can_promote boolean NOT NULL DEFAULT false,
    can_administer boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, principal_id)
);

CREATE TABLE sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    namespace_id uuid NOT NULL REFERENCES namespaces(id),
    created_by uuid NOT NULL REFERENCES principals(id),
    status text NOT NULL DEFAULT 'active',
    last_sequence bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE turns (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    sequence bigint NOT NULL,
    status turn_status NOT NULL DEFAULT 'accepted',
    actor_id uuid NOT NULL REFERENCES principals(id),
    agent_id uuid REFERENCES agents(id),
    accepted_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    completed_at timestamptz,
    UNIQUE (session_id, sequence)
);

CREATE TABLE messages (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    turn_id uuid NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('system', 'user', 'assistant', 'tool')),
    content text NOT NULL,
    content_hash text NOT NULL,
    sensitivity sensitivity_class NOT NULL DEFAULT 'internal',
    committed_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE message_chunks (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    chunk_sequence integer NOT NULL,
    content text NOT NULL,
    content_hash text NOT NULL,
    committed_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (message_id, chunk_sequence)
);

CREATE TABLE aggregate_sequences (
    aggregate_id uuid PRIMARY KEY,
    last_sequence bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE events (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    aggregate_id uuid NOT NULL,
    aggregate_sequence bigint NOT NULL,
    source text NOT NULL,
    event_type text NOT NULL,
    subject text NOT NULL,
    occurred_at timestamptz NOT NULL,
    data_schema text NOT NULL,
    correlation_id uuid,
    causation_id uuid,
    actor_id uuid REFERENCES principals(id),
    data jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (aggregate_id, aggregate_sequence)
);

CREATE INDEX events_type_time_idx ON events (event_type, occurred_at DESC);
CREATE INDEX events_data_gin_idx ON events USING gin (data);

CREATE TABLE checkpoints (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    turn_id uuid REFERENCES turns(id) ON DELETE SET NULL,
    last_event_sequence bigint NOT NULL,
    state jsonb NOT NULL,
    content_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX checkpoints_session_time_idx ON checkpoints (session_id, created_at DESC);

CREATE TABLE operations (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    turn_id uuid REFERENCES turns(id),
    tool_name text NOT NULL,
    tool_version text NOT NULL,
    request_hash text NOT NULL,
    idempotency_key text NOT NULL,
    status operation_status NOT NULL,
    request jsonb NOT NULL,
    response jsonb,
    reconciliation jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, idempotency_key)
);

CREATE TABLE idempotency_keys (
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    principal_id uuid NOT NULL REFERENCES principals(id),
    key text NOT NULL,
    operation_type text NOT NULL,
    request_hash text NOT NULL,
    status text NOT NULL,
    response_reference text,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, principal_id, key)
);

CREATE TABLE outbox_jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    operation_id text NOT NULL,
    job_type text NOT NULL,
    payload jsonb NOT NULL,
    status job_status NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_expires_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, operation_id)
);

CREATE INDEX outbox_claim_idx ON outbox_jobs (status, available_at) WHERE status IN ('pending', 'retryable');

CREATE TABLE memory_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    namespace_id uuid NOT NULL REFERENCES namespaces(id),
    owner_principal_id uuid NOT NULL REFERENCES principals(id),
    type memory_type NOT NULL,
    status memory_status NOT NULL DEFAULT 'candidate',
    title text NOT NULL,
    canonical_summary text NOT NULL,
    authority_level integer NOT NULL CHECK (authority_level BETWEEN 0 AND 100),
    confidence_score double precision NOT NULL CHECK (confidence_score BETWEEN 0 AND 1),
    sensitivity sensitivity_class NOT NULL,
    valid_from timestamptz,
    valid_until timestamptz,
    expires_at timestamptz,
    current_version_id uuid,
    superseded_by_id uuid REFERENCES memory_items(id),
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memory_versions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    memory_id uuid NOT NULL REFERENCES memory_items(id) ON DELETE CASCADE,
    version_number integer NOT NULL,
    content text NOT NULL,
    structured_content jsonb NOT NULL DEFAULT '{}'::jsonb,
    content_hash text NOT NULL,
    change_reason text NOT NULL,
    previous_version_id uuid REFERENCES memory_versions(id),
    created_by uuid NOT NULL REFERENCES principals(id),
    approved_by uuid REFERENCES principals(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    approved_at timestamptz,
    UNIQUE (memory_id, version_number)
);

ALTER TABLE memory_items
    ADD CONSTRAINT memory_items_current_version_fk
    FOREIGN KEY (current_version_id) REFERENCES memory_versions(id);

CREATE TABLE memory_evidence (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    memory_id uuid NOT NULL REFERENCES memory_items(id) ON DELETE CASCADE,
    evidence_kind text NOT NULL,
    reference_id text NOT NULL,
    content_hash text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memory_relations (
    source_memory_id uuid NOT NULL REFERENCES memory_items(id) ON DELETE CASCADE,
    relation_type text NOT NULL,
    target_memory_id uuid NOT NULL REFERENCES memory_items(id) ON DELETE CASCADE,
    confidence_score double precision NOT NULL CHECK (confidence_score BETWEEN 0 AND 1),
    valid_from timestamptz,
    valid_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source_memory_id, relation_type, target_memory_id)
);

CREATE TABLE memory_embeddings (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    memory_version_id uuid NOT NULL REFERENCES memory_versions(id) ON DELETE CASCADE,
    provider text NOT NULL,
    model text NOT NULL,
    model_revision text NOT NULL,
    dimensions integer NOT NULL,
    chunk_number integer NOT NULL,
    chunk_content_hash text NOT NULL,
    embedding vector,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (memory_version_id, model, model_revision, chunk_number)
);

CREATE TABLE memory_candidates (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    namespace_id uuid NOT NULL REFERENCES namespaces(id),
    source_event_id uuid NOT NULL REFERENCES events(id),
    proposed_type memory_type NOT NULL,
    title text NOT NULL,
    content text NOT NULL,
    authority_level integer NOT NULL CHECK (authority_level BETWEEN 0 AND 100),
    confidence_score double precision NOT NULL CHECK (confidence_score BETWEEN 0 AND 1),
    sensitivity sensitivity_class NOT NULL,
    conflict_assessment jsonb NOT NULL DEFAULT '{}'::jsonb,
    status text NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE retrieval_runs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    principal_id uuid NOT NULL REFERENCES principals(id),
    agent_id uuid REFERENCES agents(id),
    session_id uuid REFERENCES sessions(id),
    turn_id uuid REFERENCES turns(id),
    query_hash text NOT NULL,
    namespaces jsonb NOT NULL,
    filters jsonb NOT NULL,
    ranking_config jsonb NOT NULL,
    selected_memory_ids jsonb NOT NULL,
    latency_ms integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit_entries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    actor_id uuid REFERENCES principals(id),
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text NOT NULL,
    decision text NOT NULL,
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE namespaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_versions ENABLE ROW LEVEL SECURITY;

COMMIT;
