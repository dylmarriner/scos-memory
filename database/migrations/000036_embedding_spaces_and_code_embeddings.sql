BEGIN;

-- P4.2: make every persisted vector belong to an explicit embedding space.
-- A space is immutable model identity, not a mutable deployment alias. The
-- generation distinguishes intentional re-embeds of the same pinned model
-- configuration without pretending the resulting projection is canonical
-- memory/code state.
CREATE TABLE IF NOT EXISTS embedding_spaces (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    role text NOT NULL CHECK (role IN ('text', 'code', 'visual')),
    provider text NOT NULL,
    model text NOT NULL,
    model_revision text NOT NULL,
    dimensions integer NOT NULL CHECK (dimensions > 0),
    generation integer NOT NULL DEFAULT 1 CHECK (generation > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (role, provider, model, model_revision, dimensions, generation)
);

-- Existing memory_embeddings already carry provider/model/revision/dimensions.
-- Backfill each distinct legacy tuple into generation 1 before making the
-- foreign key mandatory. This keeps old BGE-small vectors addressable while
-- preventing them from ever being compared with BGE-M3 by the P4 read path.
ALTER TABLE memory_embeddings
    ADD COLUMN IF NOT EXISTS embedding_space_id uuid REFERENCES embedding_spaces(id);

INSERT INTO embedding_spaces (role, provider, model, model_revision, dimensions, generation)
SELECT DISTINCT 'text', provider, model, model_revision, dimensions, 1
FROM memory_embeddings
ON CONFLICT (role, provider, model, model_revision, dimensions, generation) DO NOTHING;

UPDATE memory_embeddings me
SET embedding_space_id = es.id
FROM embedding_spaces es
WHERE me.embedding_space_id IS NULL
  AND es.role = 'text'
  AND es.provider = me.provider
  AND es.model = me.model
  AND es.model_revision = me.model_revision
  AND es.dimensions = me.dimensions
  AND es.generation = 1;

ALTER TABLE memory_embeddings
    ALTER COLUMN embedding_space_id SET NOT NULL;

-- The legacy uniqueness key ignored embedding generation. Drop whichever
-- generated constraint name PostgreSQL assigned to that exact four-column key,
-- then make the explicit space ID part of persisted projection identity.
DO $$
DECLARE
    legacy_constraint text;
BEGIN
    SELECT con.conname
      INTO legacy_constraint
      FROM pg_constraint con
      JOIN pg_class rel ON rel.oid = con.conrelid
     WHERE rel.relname = 'memory_embeddings'
       AND con.contype = 'u'
       AND pg_get_constraintdef(con.oid) ILIKE '%memory_version_id%model%model_revision%chunk_number%'
     LIMIT 1;
    IF legacy_constraint IS NOT NULL THEN
        EXECUTE format('ALTER TABLE memory_embeddings DROP CONSTRAINT %I', legacy_constraint);
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS memory_embeddings_version_space_chunk_uidx
    ON memory_embeddings (memory_version_id, embedding_space_id, chunk_number);

CREATE INDEX IF NOT EXISTS memory_embeddings_space_version_idx
    ON memory_embeddings (embedding_space_id, memory_version_id, chunk_number);

-- Dense code vectors are projections of immutable symbol versions. tenant_id
-- is duplicated intentionally so FORCE RLS can reject cross-tenant reads and
-- writes without trusting a client-supplied repository/snapshot relationship.
CREATE TABLE IF NOT EXISTS code_embeddings (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    symbol_version_id uuid NOT NULL REFERENCES code_symbol_versions(id) ON DELETE CASCADE,
    embedding_space_id uuid NOT NULL REFERENCES embedding_spaces(id),
    chunk_number integer NOT NULL DEFAULT 0 CHECK (chunk_number >= 0),
    chunk_content_hash text NOT NULL,
    embedding vector NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (symbol_version_id, embedding_space_id, chunk_number)
);

CREATE INDEX IF NOT EXISTS code_embeddings_symbol_space_idx
    ON code_embeddings (symbol_version_id, embedding_space_id, chunk_number);
CREATE INDEX IF NOT EXISTS code_embeddings_tenant_space_idx
    ON code_embeddings (tenant_id, embedding_space_id);

ALTER TABLE code_embeddings ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_embeddings FORCE ROW LEVEL SECURITY;

CREATE POLICY code_embeddings_tenant_isolation ON code_embeddings
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
