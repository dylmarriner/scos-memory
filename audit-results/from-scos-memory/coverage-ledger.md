# Coverage ledger

Audit run: 2026-08-23. “Reviewed” means the assigned specialist traced inputs,
callers/callees, privilege assumptions, state, side effects, errors, and tests;
opening a file alone is not coverage.

| Component | Type | Entry Points | Callers | Callees | Inputs | Side Effects | Privilege | State | Dependencies | Tests | Auditor | Status | Findings |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| services/* | Go HTTP services | 18 muxes / 171 routes | compose/adapters/operators | handlers/SQL/processes/upstream HTTP | headers/JSON/env | DB/shell/files/HTTP | mixed/admin | memory/PostgreSQL | PostgreSQL/HTTP/shell | 256 Go tests | all applicable | substantial source review; no live integration | AUDIT-001..005 |
| adapters/* | Go adapters | CLI/HTTP/MCP/Hermes/model proxy | callers/tests | services/processes | command/URL/protocol data | process/network | caller-defined | local | shell/HTTP | unit tests | control/data/integration | reviewed with gaps | AUDIT-001 relationship |
| workers/* | Python workers | entrypoints/polling | compose/operator | SQL/candidate pipeline | DB rows/env | DB writes/leases | worker | durable jobs | PostgreSQL | 72 Python tests | workflow/error/persistence | source review; no live DB | test gaps |
| database/* | SQL | migrations/DB sessions | services/workers | tables/policies | tenant/principal settings | durable data/RLS | DB role | durable | PostgreSQL | governance scripts | persistence/authz | source review; no live DB | assumptions |
| deploy/* | Docker Compose | service startup/ports | operator | containers/env | environment | network/credentials/volumes | container | volumes | Docker/PostgreSQL | compose config | configuration | reviewed | AUDIT-005 |
| tests/* | test infrastructure | make/test runners | CI/operator | implementation seams | fixtures | test state | test process | ephemeral | Go/Python/Docker | mapped | test-gap | partial | missing negative integration |

Metrics: 181 tracked files; 69 Go; 24 Python; 9 SQL; 599 Go functions/methods;
171 registered HTTP routes; 256 Go tests; 72 Python test functions; 21
Dockerfiles; 6 migration/policy surfaces; 16 specialist artifacts plus the
blind-spot artifact. These are inventory counts, not semantic coverage claims.
Unresolved: service-authentication contract, live PostgreSQL/RLS, multi-service
identity propagation, queue execution, and production topology.
