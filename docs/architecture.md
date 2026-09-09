# Architecture

SCOS Memory Fabric is organized around one rule: an agent may request work, but
it does not get to define what becomes authoritative memory.

## Runtime shape

1. A client enters through an adapter or the MCP gateway.
2. The Autonomous Cognitive Control Plane journals the request and establishes
   identity, tenant, scope, and routing context.
3. Governed services perform retrieval, policy checks, tool coordination,
   extension dispatch, and code intelligence.
4. PostgreSQL records durable state. RLS policies scope tenant data at the
   database boundary.
5. Workers build derived indexes and consolidation proposals.
6. Approved proposals are written to the OKF Git store; they do not silently
   overwrite the conversational record.

## Ownership boundaries

| Concern | Authority | Derived or operational consumers |
| --- | --- | --- |
| Turns, messages, events, checkpoints | PostgreSQL | retrieval, dashboard, workers |
| Tenant isolation | PostgreSQL RLS plus request identity | service handlers |
| Search and embeddings | PostgreSQL-backed derived indexes | retrieval orchestrator |
| Curated knowledge | OKF Git store | consolidation and promotion workflows |
| Routing and tool decisions | control-plane and governance services | adapters and dashboard |

## Design constraints

- Mutating workflows use idempotency and explicit lifecycle state.
- Failure recovery is part of the workflow, not an afterthought bolted onto the
  happy path.
- Service boundaries are HTTP contracts, documented in [`api/openapi.yaml`](../api/openapi.yaml)
  and tested by [`api/contract_coverage_test.go`](../api/contract_coverage_test.go).
- The base Compose deployment fails when required secrets are absent. Production
  PKI, KMS-backed secret delivery, off-host backups, and live telemetry are
  deployment responsibilities and are not implied by a local build.

See the [ADR index](adr/) for the rationale behind the major boundaries and
[`GOVERNANCE.md`](../GOVERNANCE.md) for the normative requirements.
