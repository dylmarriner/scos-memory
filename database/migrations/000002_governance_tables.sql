BEGIN;

CREATE TABLE tool_executions (
    id uuid PRIMARY KEY,
    operation_id uuid NOT NULL REFERENCES operations(id),
    tool_identity text NOT NULL,
    declared_properties jsonb NOT NULL,
    input_hash text NOT NULL,
    expected_effects jsonb NOT NULL DEFAULT '{}'::jsonb,
    retry_policy jsonb NOT NULL DEFAULT '{}'::jsonb,
    reconciliation_method text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memory_reviews (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    candidate_id uuid NOT NULL REFERENCES memory_candidates(id),
    reviewer_id uuid REFERENCES principals(id),
    decision text NOT NULL CHECK (decision IN ('approved','rejected','quarantined','needs_changes')),
    rationale text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE okf_bundles (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    repository text NOT NULL,
    commit_hash text NOT NULL,
    bundle_hash text NOT NULL,
    status text NOT NULL CHECK (status IN ('proposed','validated','approved','diverged','rejected')),
    imported_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repository, commit_hash)
);

CREATE TABLE okf_concepts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    bundle_id uuid NOT NULL REFERENCES okf_bundles(id),
    path text NOT NULL,
    title text NOT NULL,
    content_hash text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    import_status text NOT NULL CHECK (import_status IN ('indexed','diverged','rejected')),
    UNIQUE (bundle_id, path)
);

CREATE TABLE object_references (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    namespace_id uuid REFERENCES namespaces(id),
    storage_uri text NOT NULL,
    content_hash text NOT NULL,
    media_type text NOT NULL,
    byte_size bigint NOT NULL CHECK (byte_size >= 0),
    sensitivity sensitivity_class NOT NULL,
    retention_policy jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by uuid NOT NULL REFERENCES principals(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE retention_rules (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    namespace_id uuid REFERENCES namespaces(id),
    resource_type text NOT NULL,
    policy jsonb NOT NULL,
    version integer NOT NULL CHECK (version > 0),
    approved_by uuid REFERENCES principals(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, namespace_id, resource_type, version)
);

CREATE TABLE schema_versions (
    schema_id text NOT NULL,
    version integer NOT NULL CHECK (version > 0),
    compatibility text NOT NULL CHECK (compatibility IN ('backward','forward','full','breaking')),
    registered_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (schema_id, version)
);

CREATE TABLE retrieval_results (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    retrieval_run_id uuid NOT NULL REFERENCES retrieval_runs(id) ON DELETE CASCADE,
    memory_id uuid REFERENCES memory_items(id),
    rank integer NOT NULL CHECK (rank > 0),
    lexical_score double precision,
    semantic_score double precision,
    fused_score double precision,
    provenance jsonb NOT NULL DEFAULT '{}'::jsonb,
    conflicts jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE tool_executions ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_reviews ENABLE ROW LEVEL SECURITY;
ALTER TABLE okf_bundles ENABLE ROW LEVEL SECURITY;
ALTER TABLE okf_concepts ENABLE ROW LEVEL SECURITY;
ALTER TABLE object_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE retention_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE retrieval_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE retrieval_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_entries ENABLE ROW LEVEL SECURITY;

COMMIT;
