# ADR-0001: PostgreSQL is the authoritative transactional store

- **Status:** Accepted
- **Date:** 26 July 2026
- **Decision owner:** Dylan Marriner

## Context

SCOS Memory Fabric requires crash-safe turns, append-only events, transactional checkpoints, permissions, versions, evidence, hybrid retrieval and an outbox. Splitting these responsibilities across several databases at project inception would create consistency and recovery problems before measured scale requires them.

## Decision

PostgreSQL SHALL be authoritative for interaction events, messages, checkpoints, operations, memory identity and versions, evidence, namespace policy, audit and job state.

PostgreSQL full-text search and pgvector SHALL provide the initial hybrid retrieval layer.

OKF/Git SHALL remain authoritative for approved curated knowledge. Object storage SHALL remain authoritative for large immutable artifacts.

## Consequences

- Turn completion and memory-processing jobs can commit atomically.
- Recovery has one primary transactional journal.
- Search projections can be rebuilt.
- Database capacity and WAL management become critical operating concerns.
- A separate broker, graph database or search service requires a later ADR supported by measurements.

## Compatibility

Every governed service depends on the same PostgreSQL major version
(17+) and the `pgvector` extension. Compatibility across service versions
is maintained by additive-only migrations (`database/migrations/*.sql`,
sequentially numbered, forward-only) — no service may assume a schema
version newer than the one migrations declare it applied.

## Migration

Migrations run once, in order, via `scripts/scos-migrate` before any
dependent service starts (`deploy/compose/docker-compose.yml`
`migrations` service, gated on `postgres` health). Schema changes are
additive where possible per GOVERNANCE.md §12; a field removal or meaning
change requires a new migration file, never an edit to an already-applied
one.

## Rollback

There is no automated schema-down migration. Rollback is: restore the most
recent verified base backup plus WAL replay to the point before the
migration committed (GOVERNANCE.md §31), then redeploy the prior service
images. This is intentionally the same path as disaster recovery rather
than a separate mechanism, so it is exercised by the same monthly restore
tests.

## Index rebuild

Full-text (`to_tsvector`) and `pgvector` indexes are derived and
rebuildable from `memory_versions`/`memory_embeddings` content per
GOVERNANCE.md §4.1 ("embeddings... shall be disposable and reproducible").
A rebuild re-runs embedding generation and `CREATE INDEX CONCURRENTLY`
against the authoritative rows; no authoritative data is read from the
index.

## Verification

`make validate` runs `docker compose config` against this schema plus the
structural governance audit; `go test ./database/integration -tags
integration` (see `make integration-test`) exercises RLS and append-only
guards against a live PostgreSQL container; monthly automated restore
tests verify backup/WAL recoverability per GOVERNANCE.md §31.
