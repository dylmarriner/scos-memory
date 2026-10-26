BEGIN;

-- P0-0: make RLS actually enforce. scos_app is the restricted role every
-- service connects as at runtime -- NOSUPERUSER NOBYPASSRLS, distinct from
-- the migration-owning role (POSTGRES_USER, default "scos") that creates
-- and owns every table. Table owners are exempt from RLS by default
-- regardless of policies present; only a non-owner, non-bypass role plus
-- FORCE ROW LEVEL SECURITY (000012_force_rls.sql) actually binds the
-- policies already defined across this directory.
--
-- This is the same NOSUPERUSER NOBYPASSRLS pattern already proven correct
-- in database/integration/rls_test.go's per-test fixture role, generalized
-- from a throwaway per-test schema to the real "public" schema every
-- migration writes into (confirmed: no CREATE SCHEMA anywhere under
-- database/migrations/).
--
-- The role's password is supplied by the caller via psql -v
-- app_password=... (see scripts/scos-migrate) -- required, not defaulted,
-- so this file fails loudly rather than creating a role with a guessable
-- password if the operator forgot to set SCOS_APP_PASSWORD.
-- Built and \gexec'd at the top level, not inside a DO $$ ... $$ block: psql
-- does not expand :'variable' references inside dollar-quoted text, so the
-- previous DO-block form sent the literal string ":'app_password'" to the
-- server and failed with "syntax error at or near ':'".
SELECT format('ALTER ROLE scos_app PASSWORD %L;', :'app_password')
WHERE EXISTS (SELECT FROM pg_roles WHERE rolname = 'scos_app')
UNION ALL
SELECT format('CREATE ROLE scos_app LOGIN PASSWORD %L NOSUPERUSER NOBYPASSRLS;', :'app_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'scos_app')
\gexec

GRANT USAGE ON SCHEMA public TO scos_app;

-- Minimal DML only -- no DDL, no ALL PRIVILEGES. Matches
-- database/integration/rls_test.go's fixture grant exactly.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO scos_app;
-- Revalidation decisions are authoritative append-only reviewer evidence.
-- This must follow the blanket grants on fresh installs; migration 000070
-- also revokes these rights when upgrading an existing scos_app role.
REVOKE UPDATE, DELETE ON memory_revalidation_decisions FROM scos_app;
-- Snapshot-diff evidence is server-derived and append-only to runtime roles.
REVOKE UPDATE, DELETE ON code_snapshot_change_events FROM scos_app;
-- Privileged purge evidence and its entry point are never runtime APIs.
REVOKE ALL ON memory_revalidation_decision_purge_audit FROM scos_app;
REVOKE ALL ON FUNCTION purge_memory_revalidation_decisions_for_tenant(uuid,text) FROM PUBLIC, scos_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO scos_app;

-- Without this, every future migration that adds a table silently leaves
-- scos_app unable to touch it until someone remembers to grant it by hand
-- -- this is the safeguard against P0-0 quietly regressing one table at a
-- time. Deliberately omits "FOR ROLE <name>": scos-migrate always applies
-- this connected as the migration-owning role (whatever POSTGRES_USER is
-- configured to on this deployment -- default "scos", but operator-
-- overridable), and omitting FOR ROLE defaults ALTER DEFAULT PRIVILEGES to
-- the current (connecting) role rather than hardcoding a role name that
-- would break on a renamed owner.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO scos_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO scos_app;

COMMIT;
