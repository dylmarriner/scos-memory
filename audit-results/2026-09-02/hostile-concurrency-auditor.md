# Specialist audit artifact

Skill: hostile-concurrency-auditor
Agent: Codex coordinator
Scope: indexing, cache, memory writes, queues
Files/components inspected: `pgstore.go`, Graphify `graphify.go`, consolidation worker, outbox schema
Entry points inspected: concurrent snapshot/index and Graphify query/extract paths
Important callers/callees: handlers → DB transaction; handlers → cache check → subprocess
Workflows traced: duplicate snapshot requests; concurrent cache misses; worker lease/retry
Trust boundaries: process memory/filesystem and database transactions
Attacker-controlled inputs: repeated valid requests and timing
Invariants tested: memory write advisory locking; snapshot activation transaction
Findings: Graphify cache miss path has no visible inter-process lock and final-path atomicity proof; v2 snapshot identity is unique but duplicate indexing can still repeat work; durable indexing job/lease path is not implemented in code-intelligence
Suspicions requiring validation: partial graph visibility during concurrent extraction
Tests/commands executed: source review; Go suite pass
Coverage gaps: race/load tests and live worker failures
Areas explicitly not reviewed: broker topology
Recommended follow-up: advisory/distributed cache lock, temp output + atomic rename, durable resumable jobs
