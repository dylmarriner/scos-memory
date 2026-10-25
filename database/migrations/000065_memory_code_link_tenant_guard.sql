BEGIN;

-- P6: reject cross-tenant memory/code evidence links for all SQL writers, not
-- only the HTTP endpoint. Keep the tenant check in the writer as an earlier,
-- clearer error, but enforce it at the database boundary as well.
--
-- The function is SECURITY INVOKER: it executes with the caller's existing
-- transaction-local tenant/principal context. It does not bypass memory RLS.
CREATE FUNCTION enforce_memory_code_link_tenant()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM memory_items mi
        JOIN code_symbol_versions sv ON sv.id = NEW.code_symbol_version_id
        JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id
        JOIN code_repositories repo ON repo.id = snap.repository_id
        WHERE mi.id = NEW.fact_id
          AND mi.tenant_id = NEW.tenant_id
          AND snap.tenant_id = NEW.tenant_id
          AND repo.tenant_id = NEW.tenant_id
    ) THEN
        RAISE EXCEPTION 'memory/code link targets must belong to the same visible tenant'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER memory_code_link_tenant_guard
BEFORE INSERT OR UPDATE OF tenant_id, fact_id, code_symbol_version_id
ON memory_code_links
FOR EACH ROW EXECUTE FUNCTION enforce_memory_code_link_tenant();

COMMIT;
