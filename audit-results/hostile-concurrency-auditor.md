Skill: hostile-concurrency-auditor
Agent: coordinator-sequential
Scope: assigned specialist scope across repository boundaries.
Files/components inspected: services, adapters, workers, packages, database, deploy, schemas, policies, tests, and docs as relevant.
Entry points inspected: HTTP muxes, exported functions, worker entrypoints, SQL migrations, Makefile/Docker Compose.
Important callers/callees: service handlers to persistence/upstream/process sinks; adapters to services and shell boundaries.
Workflows traced: request, auth, authorization, state transition, worker, retry/error, deployment, and recovery paths.
Trust boundaries: network-to-service, header-to-identity, JSON-to-domain, DB-to-worker, service-to-service, container-to-host.
Attacker-controlled inputs: HTTP headers/JSON, command/working directory, IDs, tenant/principal fields, environment/config, persisted rows, external responses.
Invariants tested: tenant isolation, authenticated identity, operation ownership, safe transitions, failure consistency, least privilege as applicable.
Findings: caller-selected operation-ID collision was fixed; live duplicate/retry concurrency remains unverified.
Suspicions requiring validation: live exposure, cryptographic signature enforcement, service authentication, worker/RLS coverage.
Tests/commands executed: go test ./... (pass); static source review; no live Docker/database integration.
Coverage gaps: no runtime multi-tenant integration, race harness, live external integrations, or exhaustive function count.
Areas explicitly not reviewed: secret values, production infrastructure outside repository, untracked user changes.
Recommended follow-up: centralize auth, bind internal ports privately, add negative endpoint/failure/race tests, rerun all specialists.
