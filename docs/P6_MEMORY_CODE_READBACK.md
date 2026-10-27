# P6: bounded memory → code evidence readback

**Status (2026-10-27): implemented and verified on an isolated development branch. Not a P6 completion claim.**

## API and MCP contract

`GET /v1/evidence-links?fact_id=<UUID>&limit=25&after=<link-UUID>`
reads explicit, OKF-promotion, or inferred links already persisted in
`memory_code_links`. The endpoint is read-only; it never infers or mutates
knowledge. `fact_id` is mandatory. `limit` defaults to 25 and must be in
1..100. `after` is an optional immutable link UUID; rows are ordered by
`memory_code_links.id` with a one-extra-row check. A missing or inaccessible
fact returns 404; an accessible fact without links returns `links: []`.

Each link contains its own immutable `code_symbol_version_id`, the
corresponding repository UUID, snapshot UUID, recorded commit label and path,
plus relationship type, source, confidence, creation time and link UUID.
The commit label is **not** independently attested Git provenance. The
`next_cursor` appears only when another page exists. These are direct
one-hop links, not inferred transitive graph paths.

`scos_memory_code_links` exposes the same readback through the governed MCP
gateway with `fact_id`, optional `limit` and optional `after`. The gateway
forwards the authenticated identity, validates selectors before forwarding,
and never substitutes the current code symbol for a historical version.

## Isolation and test evidence

All reads use transaction-local tenant/principal context and the forced
`memory_items` / `memory_code_links` row-level security policies.
A query checks that the fact is visible **before** returning link data, and
joins code symbol versions through tenant-scoped repository snapshots.
No direct unauthenticated database read is introduced.

The regression suite exercises malformed, repeated and unsupported query
selectors; bounded multi-page readback of three distinct immutable symbol
versions; preservation of repository/snapshot/commit/path provenance; an
accessible fact with zero links; foreign-tenant facts; a foreign-tenant
principal; and a same-tenant principal without namespace membership.
MCP forwarding tests assert method, query and authenticated tenant/principal
headers and reject invalid selectors before network calls.

TDD evidence: the new HTTP selector tests failed with 405 before the GET
handler and passed afterward; the MCP forwarding test failed with
`unhandled tool` before dispatch implementation and passed afterward.
Code review identified an internal database error leaked through HTTP 500; a
regression reproduced the raw `sql: database is closed` disclosure RED, and
now requires a fixed public error while details are logged server-side.
The POST evidence-link writer was also found to expose raw SQL/driver errors
on HTTP 500. A closed-database regression first failed with
`begin transaction: sql: database is closed` in the response; it now passes
with a fixed public error and server-side diagnostics. Transaction, principal
context, fact lookup, insert and commit errors all use this sanitized path.
The MCP gateway also reuses its existing UUID validator.

A second RED-before-fix integrity regression exposed a mismatched code-symbol
repository identity: the schema permits a symbol version to reference a
snapshot in another repository, and the original link writer and reader
checked only snapshot/repository tenant IDs. Two disposable-PostgreSQL tests
failed before the fix: one accepted the forged cross-tenant symbol at link
creation; another returned a previously valid link after its symbol identity
drifted to a foreign repository. Both now pass. The writer and reader require
`code_symbols.repository_id = code_repositories.id`, and migration
`000077_memory_code_link_symbol_repository_guard.sql` applies the same
invariant to direct SQL link writers. The direct-SQL regression checks for
SQLSTATE 23514 rather than merely accepting an arbitrary database error.

**Additional database integrity evidence (2026-10-27):** a real PostgreSQL
RED-before-fix regression showed that `code_repository_snapshots.tenant_id`
could disagree with its owning `code_repositories.tenant_id`, even though
both UUIDs had valid foreign keys. Migration `000078_code_snapshot_tenant_fk.sql`
adds a composite repository/tenant foreign key, checks new writes and
updates, and validates existing snapshots rather than grandfathering drift.
It retains repository-deletion cascade semantics. A second disposable
PostgreSQL replay intentionally seeded one mismatched legacy snapshot: the
migration refused to validate it, and after removing **only that synthetic
test row**, a retry and a repeated apply succeeded with the composite FK
marked validated. The migration is idempotent across this recovery path.
This closes one structural
tenant-provenance mismatch, **not** direct-access code-table RLS or the wider
symbol/version lineage integrity work.

**Outstanding database isolation debt:** the pre-existing
`code_repositories`, `code_repository_snapshots`,
`code_symbols` and `code_symbol_versions` tables do not yet have
independent forced RLS.
The new endpoint uses forced memory/link RLS plus explicit tenant/repository
join predicates and has negative cross-scope tests, but this does not qualify
those legacy code tables for unrestricted direct access by the runtime role.
A disposable PostgreSQL 17 check on the non-BYPASSRLS `scos_app`
role confirmed this is an exploitable **direct SQL access boundary**, not
merely a missing schema decoration: without any tenant context,
`SELECT count(*), count(DISTINCT tenant_id) FROM code_repositories`
returned 439 rows spanning 430 fixture tenants after integration tests.
The four legacy provenance tables reported `relrowsecurity=false` and
`relforcerowsecurity=false`. The composite snapshot FK does not mitigate
this direct read exposure. **PR #92 must not merge while this review finding
remains unresolved.**

Adding forced RLS to them requires a coordinated code-indexer/runtime
transaction-context migration and integration acceptance; it is not silently
treated as completed by this endpoint.

**Incremental runtime scoping evidence (2026-10-27, still draft):**
`pgCodeStore.EnsureRepository`, `CreateSnapshot`,
`LatestIndexedBranch` and `IndexingSnapshot` now use the existing
transaction-local `scos.tenant_id` helper, and that helper rejects invalid
tenant UUIDs before starting database work. A new real-PostgreSQL integration
test creates an isolated schema with FORCE RLS on both repository and snapshot
tables, uses the restricted `scos_app` role, and verifies that the own-tenant
write/read works, pending and active snapshot discovery is tenant-scoped,
a foreign tenant cannot create or discover a snapshot, and no tenant
context leaks into subsequent pooled-connection reads. The regression failed
before the change with SQLSTATE 42501 on repository creation and passed after
the change on disposable PostgreSQL 17. A second RED case found that
unscoped `IndexingSnapshot` returned an empty result for an authorized
tenant; both snapshot-discovery reads now pass under forced RLS. A further RED→GREEN code-indexer increment moves the five HTTP
repository-ownership checks (index, scan, SCIP import, query and impact) into
transaction-local tenant context. The snapshot idempotency lookup now takes
an explicit authenticated tenant ID and runs inside the same scoped
transaction. The forced-RLS shadow-schema test proves own-tenant
repository-path and snapshot-key visibility, foreign-tenant invisibility,
invalid-tenant rejection and no connection-level context leakage. Before
implementation the test failed compilation because these scoped APIs did not
exist; after implementation it passed against a fresh disposable PostgreSQL
17/pgvector instance with 97 migration/policy scripts applied. The full
code-intelligence integration package also passed in that disposable database,
as did the uncached Go suite, Go vet and diff validation.
An additional RED→GREEN regression covered immutable historical code
queries under FORCE RLS. The baseline returned `historical snapshot not found`
for an authorized tenant because snapshot resolution used an unscoped pooled
connection. Both snapshot selection and symbol-version retrieval now execute
inside one transaction-local tenant context. The restricted-role shadow-schema
test includes FORCE RLS on all four provenance tables and checks own-tenant
historical readback, foreign-tenant denial and zero context-free symbol/version
visibility. The full code-intelligence integration package, uncached Go suite
and Go vet passed on a separate disposable PostgreSQL 17/pgvector database
with 97 migration/policy files.
**This does not activate RLS on the four production code tables:**
indexing, activation, recovery, and other symbol-version query paths still
require coordinated scoping and restricted-role integration before
migration/merge. No production migration was attempted.


The focused and full memory-intelligence integration tests passed against a
**disposable PostgreSQL 17/pgvector database**, with 75 baseline migration files plus the new 000077 integrity migration,
20 policy files and a non-BYPASSRLS `scos_app` role. The P4 CI PostgreSQL job
now runs the memory-intelligence integration suite for future PRs and main.

## Unfinished acceptance

This does **not** implement code → memory expansion, multi-hop graph
traversal, ranking from graph proximity, automatic link inference, temporal
belief-history reconstruction, or production deployment/recovery validation.
The repository/snapshot/commit values are persisted labels rather than
cryptographic Git attestations. Full P6 and P0–P8 gates remain open.
