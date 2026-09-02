# ADR-0012: Row-level security is enforced against a restricted role, not merely defined

- **Status:** Accepted
- **Date:** 2026-09-01
- **Decision owner:** Dylan Marriner

## Context

ADR-0001 established PostgreSQL as the authoritative store and implicitly assumed row-level security policies (`database/policies/*.sql`) provide database-layer tenant isolation. They did not, as deployed: no policy file set `FORCE ROW LEVEL SECURITY`, and both deployment paths (`deploy/compose/docker-compose.yml`, `deploy/native/postgres-setup.sh`) connected every service as the single migration-owning role (`POSTGRES_USER`, default `scos`), which Postgres exempts from row-level security regardless of the policies present, because it owns every table.

This was found independently while producing a competitive audit of the platform (`SCOS_MEMORY_COMPETITIVE_AUDIT_AND_MASTER_PLAN.md`), and corroborated a prior hostile-security-audit finding already in this repository (`audit-results/2026-09-02/findings.md`, AUTH-001; `audit-results/comparative-divergence-audit.md`, F-08). The only role in the codebase created with `NOSUPERUSER NOBYPASSRLS` — the attributes required for row-level security to actually bind — existed solely as a throwaway fixture in `database/integration/rls_test.go`, never wired into a real deployment. As deployed, isolation was no stronger than application-layer discipline, the same category as every competitor's `WHERE user_id = ?` filtering — worse in one respect, since it was presented as database-enforced.

Additionally: `database/migrations/000003_control_plane.sql` enabled row-level security on `capabilities` (a non-tenant, shared capability registry) without ever defining a policy for it — the one table in the entire schema where `ENABLE ROW LEVEL SECURITY` existed with zero matching `CREATE POLICY`. Forcing row-level security on this table without first closing that gap would have made it deny-all under the new restricted role, breaking capability registration across six services.

## Decision

Every service SHALL connect to PostgreSQL as `scos_app`, a `NOSUPERUSER NOBYPASSRLS` role distinct from the migration-owning role, provisioned by `database/policies/000011_app_role.sql` with the minimal DML grants the request path needs (`SELECT, INSERT, UPDATE, DELETE` on tables, `USAGE, SELECT` on sequences) and `ALTER DEFAULT PRIVILEGES` so future tables inherit those grants automatically.

Every table currently enabled for row-level security (the ~70-table set enumerated in `database/policies/000012_force_rls.sql`) SHALL also have `FORCE ROW LEVEL SECURITY` set, so policies bind even against a role that owns the table. `namespace_memberships` is excluded by design — it is the ACL table nearly every other policy subqueries to resolve tenant/namespace membership, and forcing row-level security on it would be circular.

The migration-owning role (`scos`) SHALL remain unrestricted (`CREATEROLE` on the native path, superuser under Compose's official Postgres image) so it can create `scos_app`, alter table row-security attributes, and apply future migrations — it never serves requests, so it is not itself subject to `FORCE ROW LEVEL SECURITY`'s intent.

`packages/database.NewPool` SHALL query the connecting role's `BYPASSRLS` attribute on every pool it opens and fail closed if set, so a service accidentally repointed at the owning role fails at startup rather than silently losing isolation.

## Consequences

- Row-level security policies already written against `current_setting('scos.tenant_id')`/`current_setting('scos.principal_id')` (per ADR's predecessor design, `database/policies/000001_rls.sql` onward) now actually bind at the database layer for every request-serving service.
- The migration runner and every service now authenticate as different roles with different privilege levels; `SCOS_DATABASE_URL` (owner) and `SCOS_APP_DATABASE_URL` (`scos_app`) must both be present in deployment configuration, and are no longer interchangeable.
- A future migration that adds a new tenant-scoped table must also add a `CREATE POLICY` and `FORCE ROW LEVEL SECURITY` for it in the same change — `ALTER DEFAULT PRIVILEGES` covers grants automatically, but row-security policy authorship is not something a default can supply, since it depends on the new table's actual isolation shape.
- Existing application code required no changes to its query logic: `packages/database`-consuming services already set `scos.tenant_id`/`scos.principal_id` transaction-locally before querying (confirmed across `agent-registry`, `autonomous-control-plane`, `compatibility-gateway`, `credential-vault`, `hermes-provider`, `memory-gateway`, `memory-intelligence`, `okf-synchroniser`, `recovery-orchestrator`, `security-gate`, `tool-governance`) — this change makes the database also enforce what the application already assumed was true.

## Compatibility

Requires PostgreSQL's `FORCE ROW LEVEL SECURITY` (available since Postgres 9.5) and `NOBYPASSRLS`/`CREATEROLE` role attributes (long-standing). No client-visible API or schema shape changes; this is purely a connection-identity and privilege change.

## Migration

`database/policies/000010_capabilities_rls.sql`, `000011_app_role.sql`, and `000012_force_rls.sql` apply via the existing `scripts/scos-migrate` mechanism, in that order, requiring a new `SCOS_APP_PASSWORD` environment variable at apply time. Deployment config (`docker-compose.yml`, `postgres-setup.sh`, operator-maintained systemd env files) then switches every service's `DATABASE_URL` from the owner connection to `SCOS_APP_DATABASE_URL`. The migration runner itself keeps using the owner connection throughout — it is not migrated to `scos_app`, since it needs the owner's privileges to apply schema changes at all.

## Rollback

Under Compose, the owning role is a Postgres-image bootstrap superuser and therefore never subject to `FORCE ROW LEVEL SECURITY`; rollback is repointing `DATABASE_URL` back at `SCOS_DATABASE_URL` and redeploying, no SQL to undo. Under the native deployment, the owning role is not a superuser and becomes subject to policies too once `FORCE ROW LEVEL SECURITY` is live; the break-glass there is `ALTER ROLE scos BYPASSRLS;` — one statement, instantly restores full operator access, reverted with `NOBYPASSRLS` once the underlying issue is fixed — rather than un-forcing row-level security table by table.

## Out of scope

The duplicate `000008` policy-file numbering bug (unrelated numbering cosmetics) and giving `namespace_memberships` its own row-security policy (a real, separate access-model question, not a byproduct of enforcement) are both explicitly not addressed by this ADR.
