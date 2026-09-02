# Specialist audit artifact

Skill: hostile-workflow-state-machine-auditor
Agent: Codex coordinator
Scope: snapshot, memory, job, and graph lifecycle
Files/components inspected: code snapshot enum/function, handler paths, memory statuses, outbox worker
Entry points inspected: index, activate, memory correct/forget, worker claim/complete
Important callers/callees: HTTP handlers → persistence; outbox → consolidation
Workflows traced: queued/scanning/parsing/indexing/ready/active/superseded; candidate/active/superseded/deleted
Trust boundaries: caller-selected commit/branch and worker payload
Attacker-controlled inputs: replay, out-of-order activation, incomplete file list
Invariants tested: active snapshot switch is transactional
Findings: state enum exists, but no enforced transition guard and no completeness gate before activation; current handler can promote a partial supplied manifest
Suspicions requiring validation: duplicate terminal worker effects
Tests/commands executed: source/schema review; Go suite pass
Coverage gaps: adversarial transition integration tests
Areas explicitly not reviewed: live queue
Recommended follow-up: state-transition constraints, manifest checksum, job idempotency key bound to snapshot
