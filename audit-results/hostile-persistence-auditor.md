# Specialist audit artifact

```text
Skill: hostile-persistence-auditor
Agent: hostile-persistence-auditor-workflow-agent
Scope: database/migrations/*.sql and database/policies/*.sql for every table -- RLS presence,
tenant_id enforcement, soft-delete/restore correctness, cascade behavior, and any tenant-scoped
table missing RLS; cross-checked against services/hermes-provider, services/memory-intelligence,
services/autonomous-control-plane, and workers/consolidation which write to these tables.

Files/components inspected:
- database/migrations/000001_initial.sql .. 000020_dream_merge_synthesis.sql (all 20 files, full read)
- database/policies/000001_rls.sql .. 000009_hierarchy_nodes_rls.sql (all 10 files, full read) + README.md
- database/integration/rls_test.go (role/grant fixture, lines ~205-225)
- scripts/scos-migrate (migration/policy runner, full read)
- deploy/native/postgres-setup.sh (role/DB bootstrap, `CREATE DATABASE ... OWNER $POSTGRES_USER`)
- deploy/compose/docker-compose.yml (grep for DATABASE_URL / migrations service wiring)
- services/hermes-provider/cmd/server/store.go, lifecycle.go
- services/memory-intelligence/cmd/server/store.go (forgetFact, correctFact, loadFactHistory, insertFact)
- services/autonomous-control-plane/cmd/server/main.go (routing_decisions read/write)
- services/okf-synchroniser/cmd/server/proposals.go (grep only, okf_governance_roles reference)
- workers/consolidation/src/scos_consolidation/synthesis.py (memory_synthesis_state read/write)
- GOVERNANCE.md (grep for RLS/table-owner/FORCE ROW LEVEL SECURITY references)

Entry points inspected:
- Every `CREATE TABLE` in database/migrations and every `CREATE POLICY` / `ENABLE ROW LEVEL
  SECURITY` in database/policies, cross-referenced table-by-table.
- Migration/policy execution path: scripts/scos-migrate, invoked identically by the `migrations`
  compose service and (implicitly) by deploy/native install, both using SCOS_DATABASE_URL.
- Application write paths: forgetFact/correctFact (soft delete + supersession), upsertMemory
  (hermes-provider), routing_decisions insert/read (autonomous-control-plane), memory_synthesis_state
  upsert (consolidation worker).

Important callers/callees:
- scripts/scos-migrate -> psql "$SCOS_DATABASE_URL" -> runs every file in database/migrations/*.sql
  then database/policies/*.sql, in the same DB session/role as application services (docker-compose.yml
  wires the same SCOS_DATABASE_URL to the `migrations` service and to every application service).
- deploy/native/postgres-setup.sh -> `CREATE DATABASE $POSTGRES_DB OWNER $POSTGRES_USER` where
  $POSTGRES_USER (default `scos`) is also the role in SCOS_DATABASE_URL used by scos-migrate and by
  every service. No script anywhere reassigns table ownership or issues `ALTER TABLE ... OWNER TO`
  to a distinct, restricted role.
- database/integration/rls_test.go creates a *separate* role (`f.role`, NOSUPERUSER NOBYPASSRLS) via
  GRANT (not ownership transfer) specifically so its RLS assertions are meaningful -- this differs
  from the deployed topology, where the connecting role owns the tables it queries.

Workflows traced:
1. Deployment bootstrap: postgres-setup.sh / docker-compose creates DB owned by `scos` -> scos-migrate
   (as `scos`) creates every table (making `scos` the owner of each) -> scos-migrate (as `scos`) applies
   every `ENABLE ROW LEVEL SECURITY` / `CREATE POLICY` statement -> every application service connects
   as `scos` via the same SCOS_DATABASE_URL and issues normal SQL.
2. scos_forget / correctFact soft-delete and supersession chain (memory-intelligence/store.go:596-664):
   UPDATE status='revoked' scoped by id+tenant_id, then cascades backward through superseded_by_id
   links, also tenant-scoped in the SELECT that discovers each predecessor.
3. Cross-service memory-version read path: memory_items -> memory_versions -> memory_embeddings, and
   memory_items -> memory_evidence / memory_relations, each guarded (on paper) by an RLS policy.
4. routing_decisions dual-definition: migration 000003 creates routing_decisions (tenant_id, actor_id,
   jsonb columns) -> migration 000004's `CREATE TABLE IF NOT EXISTS routing_decisions` is a no-op
   because the table already exists -> migrations 000004/000006 `ALTER TABLE ... ADD COLUMN IF NOT
   EXISTS orchestration_run_id, decision_id` reconcile only those two columns -> autonomous-control-plane
   writes/reads using exactly the reconciled (000003 + those two columns) shape, not the columns unique
   to 000004's dead CREATE TABLE body (confirmation_required, context_capsule_id, outcome).
5. memory_synthesis_state cadence tracking: consolidation worker SELECTs/UPSERTs keyed by namespace_id
   with no tenant_id column on the table and no RLS policy at all protecting it.

Trust boundaries:
- Postgres role boundary: SCOS_DATABASE_URL role vs. `scos.tenant_id`/`scos.principal_id` transaction-
  local settings the gateway is expected to set per request (packages/auth middleware, per background
  context). RLS policies are the DB-side half of this boundary; app-layer WHERE clauses are the other.
- Table-owner vs. non-owner role boundary inside Postgres itself: RLS policies are enforced only against
  non-owner, non-superuser, non-BYPASSRLS roles unless `FORCE ROW LEVEL SECURITY` is set.
- Tenant boundary inside a single shared database (all tenants share one schema, isolated only by
  tenant_id column + RLS/app-layer filtering -- there is no per-tenant database or schema).

Attacker-controlled inputs:
- tenant_id / principal_id claims arriving via the request context that mcp-gateway/services turn into
  `set_config('scos.tenant_id', ...)` / `set_config('scos.principal_id', ...)` (out of this specialist's
  direct scope, but the value these settings have depends entirely on RLS actually being enforced).
- namespace_id, memory_id, factID path/body parameters used in soft-delete, correction and supersession
  queries.

Invariants tested:
- "A tenant-scoped table has RLS enabled AND a policy that filters by tenant_id (directly or via a
  tenant-scoped join), AND that policy is actually enforced against the role the application connects
  as." -- verified false for the connecting role in the deployed topology (see Finding 1).
- "A policy that gates access via `EXISTS (SELECT 1 FROM parent_table WHERE parent.id = child.fk)`
  must also constrain the parent by tenant/namespace, or the EXISTS is tautological given the FK."
  -- verified false for memory_versions, memory_evidence, memory_relations, memory_embeddings
  (Finding 2).
- "Every table holding tenant-scoped data has RLS enabled." -- verified false for
  memory_synthesis_state (Finding 3).
- "Soft-delete (scos_forget) and its cascade cannot revoke a row belonging to a different tenant."
  -- verified true: both the direct UPDATE and each step of the cascade walk are tenant_id-scoped in
  the query that discovers the row before it is mutated (memory-intelligence/store.go:617-620, 642-645).
- "A dual/duplicate CREATE TABLE across migrations (000003 vs 000004 routing_decisions) does not leave
  the application writing to columns that don't exist." -- verified true: application code
  (autonomous-control-plane/main.go) targets exactly the reconciled 000003+000006 column set.

Findings:

1. [CRITICAL] Row-Level Security is not enforced for the role every service actually connects as,
   because that role owns every table it created and no table anywhere sets
   `FORCE ROW LEVEL SECURITY`.
   - Evidence: deploy/native/postgres-setup.sh runs
     `CREATE DATABASE $POSTGRES_DB OWNER $POSTGRES_USER` (default `scos`). scripts/scos-migrate
     connects with `psql "$SCOS_DATABASE_URL"` and is the process that executes every
     `CREATE TABLE` in database/migrations/*.sql -- so `scos` (or whatever principal
     SCOS_DATABASE_URL authenticates as) owns every table. deploy/compose/docker-compose.yml wires
     the identical `SCOS_DATABASE_URL` to the `migrations` service (line 311/338/etc.) and to every
     application service (hermes-provider, memory-intelligence, autonomous-control-plane, etc.),
     so the same role that owns the tables is the role every service issues application SQL as.
     Postgres exempts the table owner from RLS policies by default; only
     `ALTER TABLE ... FORCE ROW LEVEL SECURITY` removes that exemption, and no migration or policy
     file in database/migrations or database/policies contains that clause
     (`grep -rn "FORCE ROW LEVEL SECURITY" database/` returns nothing).
   - Consequence: every `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` / `CREATE POLICY` in
     database/policies/*.sql -- the entire tenant-isolation mechanism the codebase documents as its
     database-layer backstop (GOVERNANCE.md:2673 "Memory is namespace-scoped and RLS-tested") -- is
     inert against the actual application connection in the deployed compose/native topologies. Tenant
     isolation for every table depends solely on application code remembering to add
     `WHERE tenant_id = $N` (or the namespace/session-join equivalent) to every query; a single missed
     filter in any service is a full cross-tenant read/write with zero database-level backstop, and RLS
     provides none of the defense-in-depth the migrations/policies comments claim.
     database/policies/README.md explicitly states the intended model ("PostgreSQL therefore denies
     access until application roles and transaction-local identity settings are defined") which is not
     what happens once the connecting role is also the owner.
   - Note on why this was not caught by testing: database/integration/rls_test.go deliberately creates
     a *separate*, non-owning role (`NOSUPERUSER NOBYPASSRLS`, granted access via `GRANT`, not made the
     owner) specifically to test policies -- so the test suite's role topology differs from the
     deployed role topology, and passing tests give no signal about the owner-bypass condition.
   - Confidence: Confirmed (reasoning from documented PostgreSQL RLS semantics -- table owners bypass
     RLS unless FORCE ROW LEVEL SECURITY is set -- applied to the concrete, read deploy scripts and
     compose wiring; not executed against a live database in this pass, see Coverage gaps).

2. [HIGH] Four RLS policies use a tautological `EXISTS` check against the parent table's primary key
   that provides no tenant, namespace, or principal filtering at all -- would grant unrestricted
   cross-tenant access to historical memory content even if Finding 1 is fixed.
   - `memory_versions_namespace_access` (database/policies/000001_rls.sql:36-37):
     `FOR SELECT USING (EXISTS (SELECT 1 FROM memory_items i WHERE i.id = memory_versions.memory_id))`.
     Since `memory_versions.memory_id` is `NOT NULL REFERENCES memory_items(id)`, this EXISTS is true
     for every row by referential-integrity construction -- it is not a real access check. This is the
     *only* SELECT policy on memory_versions (grep confirms no other policy file touches it), so any
     principal, regardless of tenant, namespace membership, or sensitivity ceiling, can read the full
     historical content (`content`, `structured_content`) of every memory version in the database.
   - `memory_evidence_item_access` (database/policies/000003_data_plane_rls.sql:73-75): same pattern,
     `FOR ALL` (covers SELECT/INSERT/UPDATE/DELETE), `EXISTS (... FROM memory_items i WHERE i.id =
     memory_evidence.memory_id)` -- also tautological given the FK, and `FOR ALL` means it doesn't even
     have the excuse of being read-only.
   - `memory_relations_item_access` (same file, lines 76-78): identical pattern on
     `memory_relations.source_memory_id`, `FOR ALL`.
   - `memory_embeddings_item_access` (same file, lines 79-81): identical pattern joining to
     `memory_versions` instead of `memory_items`, `FOR ALL` -- grants unrestricted read/write of vector
     embeddings (which encode memory content) across all tenants.
   - Contrast with the correctly-written sibling policies in the same files: `memory_items_namespace_access`,
     `memory_candidates_namespace_access`, `memory_reviews_candidate_access`, `checkpoints_session_access`,
     etc. all join back to a tenant_id column or a namespace_memberships/principal check. The four
     policies above are the only ones in the whole policy set that omit that join, strongly suggesting
     an oversight (the join was written to establish "this child row has a valid parent" but never
     extended to "and the caller may access that parent") rather than an intentional design choice.
   - Confidence: Confirmed (direct read of every policy file touching these four tables; no other
     restricting policy exists for them).

3. [MEDIUM] `memory_synthesis_state` (migrations/000020_dream_merge_synthesis.sql:20-25) is a
   tenant-scoped table (keyed by `namespace_id uuid REFERENCES namespaces(id)`, and namespaces are
   tenant-owned) with **no RLS enabled at all** -- no `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` and
   no policy anywhere in database/policies (`grep -rn "memory_synthesis_state" database/policies/`
   returns nothing). It is read and written by
   workers/consolidation/src/scos_consolidation/synthesis.py (`_due_for_synthesis`,
   `_mark_synthesis_run`), which connects with a plain SQL query with no tenant filter at all
   (`SELECT last_run_at FROM memory_synthesis_state WHERE namespace_id = %s` -- no tenant_id column
   exists to filter on). Given the deployed topology in Finding 1, this gap does not currently make
   things worse in practice (RLS on the tenant-scoped tables is already inert for the connecting role),
   but it breaks the schema's own stated pattern (every other tenant-relevant table added since
   migration 000009 gets an RLS policy in the same or a following policies/*.sql file) and would leave
   this table wide open even after Finding 1 is remediated, unless it is separately fixed. Impact if
   read cross-tenant is currently limited to namespace synthesis cadence/pattern-count metadata, not
   memory content, so this is Medium rather than Critical/High on its own.
   - Confidence: Confirmed (no RLS statement exists for this table anywhere in database/).

4. [LOW / Hardening] `okf_governance_roles` (migrations/000019_okf_governance_lifecycle.sql:39-52) has
   RLS enabled and exactly one policy,
   `okf_governance_roles_authenticated_access ... FOR SELECT USING (...)` -- there is no INSERT,
   UPDATE, or DELETE policy for this table anywhere. Under normal (non-owner) RLS enforcement this
   would make it impossible for any non-owner, non-bypass role to ever assign or revoke a governance
   role through ordinary application SQL; today it is masked by Finding 1 (the connecting role owns the
   table and bypasses RLS for all commands), so writes silently succeed via the ownership bypass rather
   than via an intended write policy. This should be fixed independently so it doesn't surface as a
   production outage the moment Finding 1 is remediated (e.g. via FORCE ROW LEVEL SECURITY or a
   role-ownership change).
   - Confidence: Confirmed (read of the full policy file; no INSERT/UPDATE/DELETE policy exists).

Suspicions requiring validation:
- Whether any production/staging deployment of this system actually issues
  `ALTER TABLE ... OWNER TO <non-owning-app-role>` or `FORCE ROW LEVEL SECURITY` outside of what is
  checked into this repository (e.g. via an external Terraform/Ansible/DBA runbook not present in the
  scos-memory-fabric tree). If such a step exists elsewhere, Finding 1 would not apply to that
  deployment, but nothing in database/, deploy/, or scripts/ in this repo performs it, and
  deploy/native/postgres-setup.sh explicitly makes the connecting role the DB owner.
- Whether services/okf-synchroniser/cmd/server/proposals.go (only grepped, not fully read this pass)
  writes to okf_governance_roles through a superuser/bootstrap-only code path that would make Finding 4
  intentional rather than an oversight -- worth a follow-up read.

Tests/commands executed:
- `find database/migrations database/policies -type f` and `wc -l` for inventory.
- `grep -rn "memory_versions\|memory_evidence\|memory_relations\|memory_embeddings"
  database/policies/` to confirm the tautological policies are each table's *only* policy.
- `grep -rn "FORCE ROW LEVEL SECURITY\|BYPASSRLS" database/ deploy/ scripts/` -- zero matches for
  FORCE ROW LEVEL SECURITY anywhere; BYPASSRLS appears only in the negative
  (`NOSUPERUSER NOBYPASSRLS`) inside the integration test's role-creation fixture.
  Ran within the audited repository only; no separate database was stood up to execute a live
  `psql` reproduction of the owner-bypass or the tautological-policy read in this pass (see Coverage
  gaps).
- `grep -n "OWNER TO\|REASSIGN OWNED\|ALTER DEFAULT PRIVILEGES" database/ deploy/ scripts/` -- no
  matches, confirming nothing in the repo transfers table ownership away from the migrating/connecting
  role.
- `grep -n "DATABASE_URL" deploy/compose/docker-compose.yml` -- confirmed `SCOS_DATABASE_URL` is the
  single credential wired to the `migrations` service and to every application service.
- Manual trace of forgetFact/correctFact tenant scoping in
  services/memory-intelligence/cmd/server/store.go:596-664 -- confirmed no cross-tenant soft-delete
  bypass (the cascade's second UPDATE, which lacks an explicit tenant_id filter, operates only on an id
  already proven tenant-scoped by the SELECT that produced it in the same transaction).
- Manual trace of the routing_decisions dual-schema migrations (000003, 000004, 000006) against
  services/autonomous-control-plane/cmd/server/main.go's actual INSERT/SELECT column lists -- confirmed
  no drift-induced runtime bug (non-issue, reconciliation is correct).

Coverage gaps:
- No live PostgreSQL instance was provisioned to empirically confirm the table-owner RLS-bypass
  behavior (Finding 1) or to attempt an actual cross-tenant SELECT against memory_versions
  (Finding 2) end-to-end; both findings rest on (a) documented, well-established PostgreSQL RLS
  semantics and (b) a complete read of every relevant SQL/deploy/script file, not on executing a
  reproduction. A follow-up with a disposable Postgres instance running
  `database/migrations/*.sql` + `database/policies/*.sql` as the deploy-scripted owner role, then
  attempting a cross-tenant SELECT as that same role, would upgrade this from Confirmed-by-reasoning
  to Confirmed-by-reproduction.
- Did not read services/okf-synchroniser/cmd/server/proposals.go in full (only grepped for table
  names), so cannot rule out a superuser/bootstrap write path that would make Finding 4 intentional.
- Did not inspect Kubernetes/Helm or any other deployment mechanism beyond deploy/compose and
  deploy/native -- if a separate deployment path reassigns table ownership or sets
  FORCE ROW LEVEL SECURITY, Finding 1 would need to be scoped to "compose and native only."
  `find` for such manifests in this repo returned none, but an out-of-repo deployment definition could
  exist.
- Did not review packages/auth/middleware.go's actual `set_config('scos.tenant_id', ...)` call sites
  in this pass (referenced only via the task's background context) -- did not independently verify
  every service sets that transaction-local value before every query; this is more relevant to the
  authorization/control-flow specialists' scope but is the other half of the trust boundary Finding 1
  documents.
- Did not audit non-tenant-scoped platform tables (okf_concepts, okf_bundles, okf_proposals,
  schema_versions, capability_registry, capabilities) beyond confirming their principal-gated or
  ungated design is intentional per in-file comments; did not attempt to find an unintended path by
  which tenant-scoped data could leak into them.
- Did not review the full set of Go/Python callers for every table in the schema (60+ tables) --
  focused on the tables most likely to hold sensitive tenant content (memory_items/versions/evidence/
  relations/embeddings) and the ones flagged by structural read of the policy files themselves, per
  the scope hint's emphasis on "every table" for RLS presence/tenant_id enforcement specifically.

Areas explicitly not reviewed:
- Index/constraint completeness and query-performance implications (out of security scope).
- The rest of the persistence-auditor's nominal charter (ORM/raw-SQL injection surface, transaction
  boundary correctness beyond soft-delete/restore, historical-state / append-only trigger correctness
  beyond confirming the triggers exist and are wired) was only spot-checked, not exhaustively covered,
  given the explicit scope hint to prioritize RLS/tenant_id/soft-delete/cascade across
  database/migrations and database/policies.
- authorization/control-flow/business-logic implications of Finding 1 and 2 (e.g. what an attacker with
  a valid-but-wrong-tenant session token could actually do end-to-end) are left to the
  hostile-authorization-auditor / hostile-exploit-chain-auditor per the escalation-conditions in this
  skill's charter -- this artifact documents the persistence-layer defect itself.

Recommended follow-up:
1. Escalate Finding 1 and Finding 2 to the master auditor immediately -- both are cross-tenant data
   exposure at the persistence layer, the highest-priority escalation condition in this skill's charter.
2. Fix Finding 1 by adding `ALTER TABLE <table> FORCE ROW LEVEL SECURITY;` for every RLS-enabled table
   (or, more robustly, provision a distinct non-owning application role for all services and reserve the
   owning/migrating role for schema changes only), then add an integration test that connects as the
   *actual configured application role* (not a separately-provisioned test role) and asserts a
   cross-tenant SELECT returns zero rows.
3. Fix Finding 2 by rewriting the four tautological policies to join back to tenant_id/namespace
   membership the same way `memory_items_namespace_access` and `memory_candidates_namespace_access`
   already do, e.g. for memory_versions:
   `EXISTS (SELECT 1 FROM memory_items i JOIN namespace_memberships m ON m.namespace_id = i.namespace_id
   WHERE i.id = memory_versions.memory_id AND i.tenant_id = current_setting('scos.tenant_id', true)::uuid
   AND m.principal_id = current_setting('scos.principal_id', true)::uuid AND m.can_read)`, and the
   analogous fix for memory_evidence/memory_relations/memory_embeddings.
4. Add RLS to memory_synthesis_state (Finding 3), scoped through namespaces.tenant_id, and add the
   missing INSERT/UPDATE/DELETE policy for okf_governance_roles (Finding 4) once Finding 1 is fixed so
   role assignment doesn't silently start failing.
5. Add a standing integration test (extending database/integration/rls_test.go) that specifically
   creates its test role via `ALTER TABLE ... OWNER TO` parity with the real deployment (or otherwise
   asserts the deployed role is not a table owner / has BYPASSRLS unset and FORCE ROW LEVEL SECURITY
   set), so this class of gap cannot regress silently again.
```
