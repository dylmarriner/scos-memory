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

The focused and full memory-intelligence integration tests passed against a
**disposable PostgreSQL 17/pgvector database**, with all 75 migration files,
20 policy files and a non-BYPASSRLS `scos_app` role. The P4 CI PostgreSQL job
now runs the memory-intelligence integration suite for future PRs and main.

## Unfinished acceptance

This does **not** implement code → memory expansion, multi-hop graph
traversal, ranking from graph proximity, automatic link inference, temporal
belief-history reconstruction, or production deployment/recovery validation.
The repository/snapshot/commit values are persisted labels rather than
cryptographic Git attestations. Full P6 and P0–P8 gates remain open.
