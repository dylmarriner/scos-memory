BEGIN;

-- P6 integrity: snapshot tenant equality alone does not establish symbol
-- ownership. code_symbol_versions.symbol_id and snapshot_id have independent
-- foreign keys, so a version may point at a symbol from another repository.
-- Revalidate the complete repository chain for every link writer.
-- This is SECURITY INVOKER, retaining the caller's RLS context.
CREATE OR REPLACE FUNCTION enforce_memory_code_link_tenant()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM memory_items mi
        JOIN code_symbol_versions sv ON sv.id = NEW.code_symbol_version_id
        JOIN code_symbols sym ON sym.id = sv.symbol_id
        JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id
        JOIN code_repositories repo ON repo.id = snap.repository_id
        WHERE mi.id = NEW.fact_id
          AND mi.tenant_id = NEW.tenant_id
          AND snap.tenant_id = NEW.tenant_id
          AND repo.tenant_id = NEW.tenant_id
          AND sym.repository_id = repo.id
    ) THEN
        RAISE EXCEPTION 'memory/code link targets must belong to the same visible tenant and repository'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

COMMIT;
