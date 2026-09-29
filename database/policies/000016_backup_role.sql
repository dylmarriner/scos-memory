BEGIN;

-- backup-manager dumps the whole database with pg_dump. Under FORCE ROW
-- LEVEL SECURITY (000012) even the owning role is filtered by the
-- tenant/principal policies, and pg_dump refuses outright ("query would be
-- affected by row-level security policy"), so no backup could run.
--
-- scos_backup is a read-only login role with its own password, supplied by
-- the caller via psql -v backup_password=... (see scripts/scos-migrate),
-- required and never defaulted -- the same contract as scos_app (000011) and
-- scos_worker (000015). It is NOBYPASSRLS so it works on managed PostgreSQL:
-- whole-table reads come from role-scoped SELECT policies, and pg_dump runs
-- with --enable-row-security so those policies are honoured rather than
-- rejected. It holds no write privilege anywhere.
SELECT format('ALTER ROLE scos_backup PASSWORD %L;', :'backup_password')
WHERE EXISTS (SELECT FROM pg_roles WHERE rolname = 'scos_backup')
UNION ALL
SELECT format('CREATE ROLE scos_backup LOGIN PASSWORD %L NOSUPERUSER NOBYPASSRLS NOCREATEROLE;', :'backup_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'scos_backup')
\gexec

GRANT USAGE ON SCHEMA public TO scos_backup;

-- Grants SELECT on every table and sequence in public and a whole-table
-- SELECT policy on every table with row security enabled. Idempotent;
-- scripts/scos-migrate calls it after every run so tables created by later
-- migrations are included -- a table the backup role cannot read would make
-- pg_dump fail, never silently drop from the backup.
CREATE OR REPLACE FUNCTION scos_reconcile_backup_role() RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    t text;
BEGIN
    EXECUTE 'GRANT SELECT ON ALL TABLES IN SCHEMA public TO scos_backup';
    EXECUTE 'GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO scos_backup';
    FOR t IN
        SELECT c.relname FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND c.relrowsecurity
    LOOP
        EXECUTE format('DROP POLICY IF EXISTS scos_backup_read ON public.%I', t);
        EXECUTE format('CREATE POLICY scos_backup_read ON public.%I FOR SELECT TO scos_backup USING (true)', t);
    END LOOP;
END
$$;

SELECT scos_reconcile_backup_role();

COMMIT;
