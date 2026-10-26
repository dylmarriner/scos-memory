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

**Outstanding database isolation debt:** the pre-existing
`code_repositories`, `code_repository_snapshots` and
`code_symbol_versions` tables do not yet have independent forced RLS.
The new endpoint uses forced memory/link RLS plus explicit tenant/repository
join predicates and has negative cross-scope tests, but this does not qualify
those legacy code tables for unrestricted direct access by the runtime role.
Adding forced RLS to them requires a coordinated code-indexer/runtime
transaction-context migration and integration acceptance; it is not silently
treated as completed by this endpoint.

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
