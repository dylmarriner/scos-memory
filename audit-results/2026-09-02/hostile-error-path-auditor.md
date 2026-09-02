# Specialist audit artifact

Skill: hostile-error-path-auditor
Agent: Codex coordinator
Scope: parse, DB, subprocess, worker, and retrieval failure paths
Files/components inspected: code store/handlers, Graphify, retrieval, consolidation
Entry points inspected: malformed JSON, parser errors, DB errors, subprocess exit, missing embedder
Important callers/callees: handler → fallback/error response; worker → retry/dead-letter
Workflows traced: Tree-sitter error fallback; vector failure → lexical fallback; Graphify failure → 500
Trust boundaries: errors and partial state
Attacker-controlled inputs: malformed content/queries and repeated failures
Invariants tested: transaction rollback/atomic snapshot activation; retrieval fallback
Findings: parse failure can leave a successful file-level-only snapshot; Graphify cache output atomicity is not proven; vector fallback is honest but quality degradation is not surfaced as a typed answer-quality signal
Suspicions requiring validation: dead-letter recovery and retry idempotency
Tests/commands executed: source review; Go suite pass
Coverage gaps: fault injection against live DB/worker/subprocess
Areas explicitly not reviewed: production incident history
Recommended follow-up: index quality status, temp artifacts, resumable checkpoints, failure-injection tests
