# Specialist audit artifact

Skill: hostile-test-gap-auditor
Agent: Codex coordinator
Scope: test evidence for memory, retrieval, code graph, security, and lifecycle
Files/components inspected: service tests, integration tests, benchmarks, testdata
Entry points inspected: code-intelligence v1/v2, Graphify, tenant isolation, retrieval
Important callers/callees: HTTP handlers and parser/store helpers
Workflows traced: positive extraction/query; rename/branch fixtures; tenant mismatch
Trust boundaries: test doubles vs live Postgres/Compose
Attacker-controlled inputs: duplicate, partial, stale, cross-tenant, malformed requests
Invariants tested: current Go suite and existing regression fixtures
Findings: unit suite passes, but no benchmark proves retrieval quality, no test proves complete manifest activation, wrong symbol_count, ordinary-edit lineage, concurrent cache writers, or FORCE-RLS runtime behavior
Suspicions requiring validation: acceptance scripts require live stack and were not run
Tests/commands executed: `go test ./...` pass; static test mapping
Coverage gaps: live DB/Compose, race/load, deletion cascade, external indexer fixtures
Areas explicitly not reviewed: private production tests
Recommended follow-up: add failing regression tests for each listed invariant before feature expansion
