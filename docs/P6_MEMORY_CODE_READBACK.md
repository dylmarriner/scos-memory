# P6: bounded memory → code evidence readback

**Status (2026-10-28): merged-source readback and RLS hardening, P6 acceptance still PARTIAL.**

**Source merged (PR #92 and PR #93), checked 2026-10-28:** Merged `main`
contains `database/migrations/000079_code_provenance_force_rls.sql`
(four provenance tables) and `000080_derived_code_force_rls.sql`
(five derived-source/graph tables). Both enable and FORCE tenant RLS;
restricted-role PostgreSQL qualification is required in the P4 CI workflow.
The merged-main CI and P4 workflows passed on commit
`8e13cf60f9a85cbe6f64be41fca8c2a1af9764eb` on 2026-10-27.
**Live production deployment is not established** for either migration:
merging source and passing disposable-database tests do not apply policies
to the live database. Startup recovery/maintenance qualification, governance
§41, server-attested provenance, representative performance and P0–P8
acceptance remain open. Dated pre-merge development notes below describe
historical checkpoints, not the current merge state.


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

**Historical database isolation debt (before migration 000079):** the pre-existing
`code_repositories`, `code_repository_snapshots`,
`code_symbols` and `code_symbol_versions` tables did not yet have
independent forced RLS at that checkpoint. The source migration has since merged.
The new endpoint uses forced memory/link RLS plus explicit tenant/repository
join predicates and has negative cross-scope tests, but this does not qualify
those legacy code tables for unrestricted direct access by the runtime role.
A disposable PostgreSQL 17 check on the non-BYPASSRLS `scos_app`
role confirmed this is an exploitable **direct SQL access boundary**, not
merely a missing schema decoration: without any tenant context,
`SELECT count(*), count(DISTINCT tenant_id) FROM code_repositories`
returned 439 rows spanning 430 fixture tenants after integration tests.
At that checkpoint the four legacy provenance tables reported
`relrowsecurity=false` and `relforcerowsecurity=false`. The composite
snapshot FK did not mitigate that direct read exposure. **PR #92 was held
until the forced-RLS migration and qualification resolved the finding.**

Adding forced RLS required coordinated code-indexer/runtime
transaction-context changes and integration acceptance. Those source changes
subsequently merged; the endpoint alone was never sufficient evidence.

**Incremental runtime scoping evidence (2026-10-27, pre-merge checkpoint):**
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
Current exact path/FQN and lexical code retrieval now have explicit
tenant-scoped variants, and the HTTP query handler uses those variants.
A new RED-before-fix test failed compilation because the scoped methods
did not exist, then passed against the restricted runtime role and four
FORCE-RLS shadow tables. It checks the authorized exact and lexical symbol
version, foreign-tenant empty results and context-free invisibility. The
existing owner-level methods remain for legacy internal callers and tests;
impact traversal and other code-indexer paths still require conversion.
The snapshot-change-event HTTP readback now resolves its snapshot identity
and reads its event ledger in **one transaction-local tenant context**. A
restricted `scos_app` integration regression with four forced-RLS shadow
provenance tables first failed RED (`404 snapshot not found` for the authorized
tenant), then passed GREEN for authorized access and foreign-tenant 404.
Fresh disposable PostgreSQL 17/pgvector applied 97 migration/policy scripts;
the complete code-intelligence PostgreSQL integration package, uncached
`go test -count=1 ./...`, `go vet ./...`, and `git diff --check` passed.
The next TDD increment converts scan/indexing status transitions to
`SetSnapshotStatusScoped`, failed-snapshot cleanup to
`DeleteFailedSnapshotScoped`, and idempotent snapshot file-manifest
comparison to `SnapshotHoldsFilesScoped`. Each uses the authenticated
tenant in a transaction-local context; the manifest lookup first checks
snapshot ownership, so an empty file list cannot authorize a foreign
snapshot. The new forced-RLS regression was RED at
compile time before implementation, then GREEN for own-tenant updates/deletes,
foreign-tenant rejection of both operations, invalid tenant IDs and no
pooled-connection context leakage. A second real-PostgreSQL test covers the production
`code_snapshot_status` enum and an actual indexed file manifest, including
foreign-tenant denial and mismatch handling, using the non-BYPASSRLS role.
On a disposable PostgreSQL 17/pgvector database with 97 migrations
and policies, the full code-intelligence integration suite, uncached
Go suite, vet and diff checks passed. The old unscoped helper remains
for legacy internal tests; this change does **not** make all indexer
operations safe under future forced RLS.
A further RED→GREEN TDD increment scopes **snapshot reactivation** to the
authenticated tenant. Both scan and synchronous index-retry paths now use
`ReactivateSnapshotScoped`, which validates snapshot ownership and performs
the status lock and activation inside the same transaction-local tenant context.
The previous unscoped helper remains only for legacy internal callers/tests.
The restricted `scos_app` forced-RLS shadow-schema regression verifies an
authorized active-snapshot retry, foreign-tenant denial and malformed-tenant
rejection; a real-schema PostgreSQL regression additionally exercises an
authorized superseded-generation reactivation and foreign-tenant rejection.
The tests failed at compile time before the scoped method existed, then passed
after implementation. On disposable PostgreSQL 17/pgvector with all 97
migration/policy scripts, the complete code-intelligence integration package,
uncached `go test -count=1 ./...`, `go vet ./...`, and diff checks passed.
This is a scoped activation-path improvement, **not** a four-table RLS rollout.
A subsequent RED→GREEN regression scopes **snapshot diff readback** and
the scan/indexing response's diff to the authenticated tenant. The prior
HTTP route used a context-free snapshot lookup and returned 404 for its
owner under forced RLS. The new `DiffSnapshotScoped` validates snapshot
ownership and parent repository/tenant lineage, then reads current and
parent symbol versions through the same transaction-local tenant context.
A restricted-role PostgreSQL 17 test with four forced-RLS shadow provenance
tables proves authorized added/modified diffs, foreign/missing 404 denial,
invalid UUID rejection and single-object JSON parsing. Legacy internal
`DiffSnapshot` remains available for owner-level test tooling. All 97
migration/policy scripts, the code-intelligence integration package,
uncached Go suite and Go vet passed in a disposable test database.
This is preparatory code hardening, **not** production forced-RLS activation.
The synchronous index and idempotent retry responses now also retrieve
snapshot symbol counts through `SnapshotSymbolCountScoped`. The prior
context-free query returned SQL "no rows in result set" for the authorized
tenant under forced RLS (RED); the scoped implementation returns the correct
count and rejects foreign or invalid tenants (GREEN). The existing test
database is disposable and production policies remain unchanged.


**P8 current and historical impact traversal hardening (2026-10-27,
PR #92, merged source):** `POST /v2/code/impact` now calls
`ChangeImpactScoped` for current snapshots. Seed lookup, every bounded
reverse-CALLS hop, graph evidence loading and PageRank all run in one
authenticated transaction-local tenant context. Historical impact now keeps
its immutable-snapshot resolution and graph traversal in the **same**
transaction rather than losing RLS identity between the two phases.
Existing owner-level helpers remain for legacy tests; they are not used
by the current HTTP route. The historical selector requirement is preserved.

A real PostgreSQL 17/pgvector regression uses an isolated shadow schema
with forced RLS on repository, snapshot, symbol, version and CALLS-edge
tables and the non-BYPASSRLS `scos_app` role. It checks a real caller edge
and its two-node evidence path for the owner, both current and historical
impact, foreign-tenant denial and invalid tenant/selector rejection.
The new test was RED before implementation because `ChangeImpactScoped`
did not exist and passed GREEN after implementation. All 97
migration/policy scripts and the full code-intelligence integration
package passed in a disposable database. This is preparatory runtime
scoping only at that checkpoint; the four legacy provenance tables
**then lacked forced RLS** (subsequently added in merged migration 000079).
File and SCIP ingestion received additional scoping in the later increments below;
recovery and other legacy paths remain unqualified. No live database was changed.

**P6 file and SCIP indexing transaction scoping (2026-10-27, PR #92, merged source):** production file indexing and SCIP import now share
transaction-local tenant context across repository/snapshot ownership,
symbol/version writes, change-ledger emission, status updates and atomic
activation. Both paths use an explicit tenant+repository+snapshot ownership
guard even before production code-table RLS is enabled. Legacy owner-level
index helpers remain available to existing migration tests, while the
runtime HTTP/scan entry points use the scoped variants. The restricted-role
event-ledger integration fixture was updated to exercise the production
scoped path rather than a context-free legacy helper.

The new real-PostgreSQL integration test was RED before the scoped APIs
existed and GREEN afterward. It exercises actual Go-source and SCIP
indexing as `scos_app`, foreign-tenant denial and absence of leaked tenant
context on the pooled connection. On a **disposable** PostgreSQL 17
instance with all 97 migration/policy files, candidate forced-RLS
policies were applied to the four code provenance tables for qualification
only. Direct `scos_app` reads with no tenant context returned
`0|0|0|0`; the new opt-in
`TestPGCodeProvenanceForcedRLSQualification` verified all four policies
were enabled/forced, denied unscoped reads and permitted scoped owner
reads. The full code-intelligence integration suite passed under these
temporary policies after its restricted-role event fixture was corrected.
The uncached full Go suite, Go vet and tagged memory-intelligence PostgreSQL
integration suite also passed on the same disposable forced-RLS database.
A further real PostgreSQL forced-RLS regression reproduced an authorized
failed-SCIP retry returning HTTP 500 because the handler's unscoped
`DeleteFailedSnapshot` silently deleted zero rows (RED). The handler now
uses `DeleteFailedSnapshotScoped`, and the same end-to-end retry returns
HTTP 200 with a new immutable snapshot ID (GREEN). This was tested with
the restricted `scos_app` role and actual SCIP symbols.

**P6 snapshot listing/detail tenant-scoped readback (2026-10-27, PR #92, merged source):** `GET /v2/code/snapshots` and
`GET /v2/code/snapshots/{snapshot_id}` now execute repository/snapshot
joins within the authenticated tenant's transaction-local context, and
require matching tenant identities on both joined records. Before the
change, a real restricted-role PostgreSQL 17 shadow-schema regression
returned HTTP 200 with an empty list to the legitimate owner (RED)
because the query ran on a context-free pooled connection. The same
regression now returns the owner's snapshot and detail (GREEN), isolates
a second tenant's own repository and snapshot, denies cross-tenant
detail reads, and checks invalid tenant contexts cannot expose driver
errors in HTTP responses. A dedicated disposable database applied all
97 migrations/policies; the complete code-intelligence integration
package, uncached Go suite, Go vet and diff validation passed.
A second pass enabled candidate FORCE RLS policies on all four provenance
tables in **that disposable database only**. The opt-in
`SCOS_EXPECT_CODE_PROVENANCE_RLS=1` check and the complete code-intelligence
integration package passed, as did tagged memory-intelligence PostgreSQL
integration. Direct context-free runtime-role reads were checked by the
qualification test. This was runtime scoping only at that checkpoint; source-level forced RLS
subsequently merged, while live production rollout remains unverified.

**P4/P6 structural retrieval tenant scoping (2026-10-27, PR #92, merged source):** the production code-query handler now calls
`QueryStructuralCandidatesScoped`. Active snapshot resolution, every
bounded structural-neighbor hop, edge loading and PageRank all use one
authenticated transaction-local tenant context. Neighbor versions must
belong to the resolved snapshot, preventing cross-generation graph
contamination. The owner-level helper remains for legacy internal
integration tests, not the authenticated HTTP path. A real restricted-role
PostgreSQL 17 forced-RLS shadow-schema regression was RED before the
scoped API existed and GREEN afterward, proving owner-visible caller
evidence, graph centrality, foreign-tenant denial and invalid tenant
rejection. The full code-intelligence integration suite passed with
candidate four-table forced RLS on a disposable production-schema
database, as did the uncached Go suite and Go vet. This is not
production policy activation or P4/P6 completion.

**P6 code embedding maintenance tenant scoping (2026-10-27, PR #92, merged source):** `embedCodeSnapshot` now reads immutable symbol
documents and inserts missing chunks only within transaction-local
authenticated tenant context. Active-snapshot discovery for dense
maintenance is also tenant-scoped; the original context-free helpers
remain only for legacy internal test compatibility. A restricted
`scos_app` PostgreSQL 17 forced-RLS shadow-schema regression was RED
before the scoped APIs existed and GREEN afterward. It checks
owner-visible symbol documents, foreign-tenant denial, idempotent
chunk creation, active-snapshot selection and invalid tenant UUIDs.
A second integration test exercises the real migrated schema using
the restricted role. The disposable test database applied 77
migrations and 20 policy scripts. Candidate FORCE RLS policies were then
applied to the four legacy provenance tables in that disposable database
only; the complete code-intelligence and tagged memory-intelligence
PostgreSQL integration suites passed, including the opt-in forced-RLS
qualification gate. This increment does not activate
the then-pending provenance RLS policies (now merged in source).

**P6 user settings isolation and error sanitization (2026-10-27,
PR #92, merged source):** policy 000020_code_user_settings_rls.sql
enables/forces RLS on the previously unprotected
code_intelligence_user_settings table, and both runtime settings
operations now use transaction-local authenticated tenant context.
A new restricted-role PostgreSQL regression was RED before the
runtime/policy changes (owner insert blocked, policy catalog absent)
and GREEN afterward, covering tenant and principal separation,
no-context direct read denial, invalid tenant UUIDs and pooled
connection cleanup. A separate RED-to-GREEN HTTP regression proves
settings GET/PUT and the shared route guard no longer return raw
SQL/driver error details to clients. A fresh disposable PostgreSQL 17/pgvector database applied all 98
migration/policy scripts and passed the complete code-intelligence
integration package, uncached Go suite (51 tested packages), Go vet
and diff checks. This new policy exists in source only; it has not
been applied to any live service.
The four provenance tables now have forced-RLS policies in merged source;
live deployment has not been verified, and startup recovery remains an open qualification gate.

**Four-table provenance RLS migration 000079 is merged, not deployed here.**
Derived-table migration 000080 is merged too. Recovery, background maintenance
and remaining callers require qualification for a controlled live rollout.

This does not make the Git commit label server-attested. Additional
recovery and maintenance paths still need RLS qualification.
**This does not establish live production RLS activation:**
merged source migrations are not a live database migration. Recovery and
maintenance paths still require rollout qualification. No production
migration was attempted in this development work.


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
