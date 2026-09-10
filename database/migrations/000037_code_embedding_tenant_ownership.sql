BEGIN;

-- P4 hardening: tenant_id on a code embedding must match the tenant that owns
-- the referenced immutable symbol version. RLS protects row visibility; this
-- trigger additionally protects referential ownership even from buggy internal
-- callers that supply mismatched tenant/symbol IDs.
CREATE OR REPLACE FUNCTION enforce_code_embedding_tenant_ownership()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM code_symbol_versions sv
        JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id
        JOIN code_repositories repo ON repo.id = snap.repository_id
        WHERE sv.id = NEW.symbol_version_id
          AND repo.tenant_id = NEW.tenant_id
    ) THEN
        RAISE EXCEPTION 'code embedding tenant does not own symbol version %', NEW.symbol_version_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS code_embeddings_tenant_ownership ON code_embeddings;
CREATE TRIGGER code_embeddings_tenant_ownership
    BEFORE INSERT OR UPDATE OF tenant_id, symbol_version_id ON code_embeddings
    FOR EACH ROW
    EXECUTE FUNCTION enforce_code_embedding_tenant_ownership();

COMMIT;
