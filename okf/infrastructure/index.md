---
title: Infrastructure
description: Approved infrastructure inventory and topology for SCOS Memory Fabric — what runs, where, and why.
tags: [infrastructure, deployment, topology]
status: active
---

# Infrastructure

This section is the approved, human-readable inventory of infrastructure
components. It documents *what is approved to run*, not live operational
state (that lives in the transparency dashboard and `/v1/transparency/*`
projections, which are derived and rebuildable, never authoritative).

## Core services

| Component | Technology | Authority | ADR |
|---|---|---|---|
| Primary transactional store | PostgreSQL 17+ with pgvector, RLS, full-text search | Authoritative for events, messages, checkpoints, memory identity/versions, evidence, namespace policy, audit, outbox | [ADR-0001](../../docs/adr/0001-postgresql-authority.md) |
| Autonomous Cognitive Control Plane | Native Go service | Mandatory ingress and orchestration authority | [ADR-0002](../../docs/adr/0002-autonomous-control-plane.md) |
| Object storage | S3-compatible | Authoritative for uploaded files, artifacts, backups | GOVERNANCE.md §7.6 |
| OKF repository | Git | Authoritative for approved curated knowledge (this repository) | GOVERNANCE.md §26 |
| Telemetry | OpenTelemetry Collector (OTLP/HTTP) | Derived/observational only | GOVERNANCE.md §32 |

## Deployment topology

Initial deployment uses Docker Compose (`deploy/compose/docker-compose.yml`)
with a per-service TLS certificate issued by the `tls-certs` bootstrap
container. Every governed HTTP service terminates TLS and refuses plaintext
fallback (`SCOS_TLS_REQUIRED=true`).

Kubernetes, a dedicated graph database, Kafka, a dedicated search cluster,
and multi-region active-active writes are explicitly deferred per
GOVERNANCE.md §36 until measured need is demonstrated and an ADR is
approved.

## Secrets and credentials

Secrets (`SCOS_INTERNAL_SERVICE_TOKEN`, `SCOS_CONTROL_PLANE_TOKEN`,
`SCOS_SECURITY_ISSUER_SECRET`, `POSTGRES_PASSWORD`, backup encryption key
references) are injected at deployment time via the environment or a secret
manager. None are committed to this repository; `.env.example` documents
the required names with empty values only.

## Backup and recovery targets

See GOVERNANCE.md §31 for the required retention schedule (35-day WAL/daily,
12-week weekly, 12-month monthly, indefinite OKF Git history). Off-host
backup provider and WAL archive destination are deployment-owned
configuration (`SCOS_BACKUP_PROVIDER`, `SCOS_WAL_ARCHIVE_DESTINATION`) and
remain unset — fail-closed — until a verified remote provider is supplied.
