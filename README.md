# SCOS Memory Fabric

A crash-resilient, permission-aware, model-neutral memory substrate for SCOS, Nexus and heterogeneous AI agents.

## Current implementation status

This repository contains the implemented baseline and its governance checks:

- The authoritative governing blueprint in `GOVERNANCE.md`.
- A minimal Go Memory Gateway with live and dependency-aware readiness endpoints.
- PostgreSQL + pgvector development infrastructure.
- Initial durable journal and memory schema.
- JSON Schemas for events, memory candidates and agent handoffs.
- An OpenAPI contract baseline.
- A Python consolidation package with validated domain models.
- OKF-compatible governing knowledge structure.
- Recovery and architecture-decision documentation.

The governance document is a mandatory production gate, not a statement that
all listed future capabilities are already complete. Run `make audit` before
describing a build as production-ready. The audit fails closed when required
services, interfaces, persistence boundaries, or test evidence are missing.

## Architecture rule

> Persist every interaction. Promote only durable knowledge.

PostgreSQL is authoritative. OKF/Git is authoritative for approved curated knowledge. Search indexes and embeddings are derived and rebuildable.

## Start development services

```bash
cp .env.example .env
docker compose -f deploy/compose/docker-compose.yml up --build
```

Gateway endpoints:

```text
GET http://localhost:8080/health/live
GET http://localhost:8080/health/ready
GET http://localhost:8080/v1/version
```

## Local checks

```bash
make fmt
make test
make validate
make audit
```

## Repository map

- `services/memory-gateway`: Go HTTP service.
- `services/transparency-projection`: permission-forwarding live dashboard projection.
- `dashboard`: browser client for authorised operational state.
- `workers/consolidation`: Python candidate models and worker foundation.
- `database/migrations`: authoritative database migrations.
- `schemas`: JSON Schema contracts.
- `api`: OpenAPI contract.
- `okf`: approved portable knowledge.
- `docs`: ADRs and runbooks.
- `deploy`: local and production deployment assets.

## Development order

1. Durable sessions, turns, messages, chunks and events.
2. Transactional checkpoints and outbox processing.
3. Namespaces, policies, memory versions and evidence.
4. Hybrid retrieval using PostgreSQL full text and pgvector.
5. Consolidation, conflict detection and review.
6. OKF import/export and protected approval workflow.
7. MCP and A2A adapters.
8. Recovery, backup and failure-injection hardening.

See `GOVERNANCE.md` before changing architecture. Future-us has enough problems without present-us inventing incompatible persistence paths.


## Autonomous operation

Every supported request enters through the Autonomous Cognitive Control Plane. It journals the request, resolves scope, retrieves memory, selects code and data capabilities, compiles context, dispatches the agent, mediates tools, commits the response and checkpoint, and triggers consolidation automatically.

Agents are workers inside the lifecycle. They are not responsible for remembering to call memory.
