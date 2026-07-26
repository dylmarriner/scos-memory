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
