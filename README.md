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

Compose requires six deployment values and does not provide secret defaults.
Copy the example, set `SCOS_INTERNAL_SERVICE_TOKEN`, `SCOS_CONTROL_PLANE_TOKEN`,
`SCOS_SECURITY_ISSUER_SECRET`, `POSTGRES_PASSWORD`, `SCOS_DATABASE_URL` (with
`sslmode=require`, `verify-ca`, or `verify-full`), and `SCOS_AGENT_ENDPOINT` in
`.env`, then validate and start the stack:

```bash
cp .env.example .env
${EDITOR:-vi} .env
set -a; . ./.env; set +a
docker compose -f deploy/compose/docker-compose.yml config >/dev/null
docker compose -f deploy/compose/docker-compose.yml up --build
```

Use `openssl rand -base64 32` for each bearer or issuer secret. Backup keys are
not configured in the base Compose file; configure the deployment secret manager
and `SCOS_BACKUP_ENCRYPTION_KEY_REF` when backups are enabled.

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

## Failure-injection acceptance probes

These are integration probes, not unit simulations. Start the complete compose
stack and source the same environment used by Compose:

```bash
cp .env.example .env
# Set SCOS_INTERNAL_SERVICE_TOKEN, SCOS_CONTROL_PLANE_TOKEN, and
# SCOS_SECURITY_ISSUER_SECRET in .env.
set -a; . ./.env; set +a
docker compose -f deploy/compose/docker-compose.yml up --build -d
make failure-injection
```

The runner executes all 58 cases against live HTTP services, terminates and
restarts compose services for crash cases, and performs duplicate/idempotency
assertions directly through the PostgreSQL container. It exits non-zero and
reports the missing dependency when the stack or service token is absent; no
case is replaced by a local or synthetic success.

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
