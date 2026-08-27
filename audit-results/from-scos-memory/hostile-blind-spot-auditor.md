Skill: hostile-blind-spot-auditor
Agent: coordinator-sequential
Scope: assigned specialist scope across repository boundaries.
Files/components inspected: services, adapters, workers, packages, database, deploy, schemas, policies, tests, and docs as relevant.
Entry points inspected: HTTP muxes, exported functions, worker entrypoints, SQL migrations, Makefile/Docker Compose.
Important callers/callees: service handlers to persistence/upstream/process sinks; adapters to services and shell boundaries.
Workflows traced: request, auth, authorization, state transition, worker, retry/error, deployment, and recovery paths.
Trust boundaries: network-to-service, header-to-identity, JSON-to-domain, DB-to-worker, service-to-service, container-to-host.
Attacker-controlled inputs: HTTP headers/JSON, command/working directory, IDs, tenant/principal fields, environment/config, persisted rows, external responses.
Invariants tested: tenant isolation, authenticated identity, operation ownership, safe transitions, failure consistency, least privilege as applicable.
Findings: blind-spot candidates AUDIT-004 and AUDIT-005 were fixed; no additional confirmed Critical issue was established.
Suspicions requiring validation: live exposure, cryptographic signature enforcement, service authentication, worker/RLS coverage.
Tests/commands executed: go test ./... (pass); static source review; no live Docker/database integration.
Coverage gaps: no runtime multi-tenant integration, race harness, live external integrations, or exhaustive function count.

Independent final pass: sampled operational recovery/backup, compatibility and
adapters, migrations/RLS, worker entrypoints, direct health/service routes, and
Compose defaults. It added AUDIT-005 and promoted operation-ID/result ownership
from a business-logic suspicion to AUDIT-004. No additional confirmed Critical
finding was established.
Areas explicitly not reviewed: secret values, production infrastructure outside repository, untracked user changes.
Recommended follow-up: centralize auth, bind internal ports privately, add negative endpoint/failure/race tests, rerun all specialists.
