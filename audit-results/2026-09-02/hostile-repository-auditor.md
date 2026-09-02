# Specialist audit artifact

Skill: hostile-repository-auditor
Agent: Codex coordinator
Scope: repository architecture, attack surface, competitive implementation boundary
Files/components inspected: services/, packages/, workers/, adapters/, integrations/, database/, deploy/, tests/, benchmarks/
Entry points inspected: HTTP muxes, MCP gateway, memory/retrieval/code/Graphify APIs, workers, CLI subprocess boundary
Important callers/callees: MCP gateway → downstream services; code-intelligence → PostgreSQL; Graphify provider → `graphify`; consolidation worker → PostgreSQL/DeepSeek
Workflows traced: memory write/retrieve/consolidate; repository register/index/query/impact; graph extract/query; auth/tenant propagation
Trust boundaries: bearer claims, service headers, JSON bodies, database role/RLS, filesystem mounts, subprocess, external LLM
Attacker-controlled inputs: request identity fields, repo paths, query/node values, file manifests/content, env/config, persisted jobs
Invariants tested: tenant binding, snapshot immutability/completeness, graph freshness, evidence provenance, derived-index rebuildability
Findings: see `findings.md`; highest are partial snapshot activation, incorrect symbol count, unstable identity, runtime RLS proof gap
Suspicions requiring validation: concurrent Graphify partial-file exposure; live role bypass/RLS behavior
Tests/commands executed: `rg`, `git ls-files`, line-numbered source review, `go test ./...` (pass)
Coverage gaps: live Compose/PostgreSQL/role tests, external indexer execution, hosted competitor internals, full 1203-file semantic trace
Areas explicitly not reviewed: secrets, private hosted services, production data
Recommended follow-up: fix P0/P1 data-integrity issues and run live RLS/Compose/indexing benchmarks
