# Specialist audit artifact

Skill: hostile-blind-spot-auditor
Agent: Codex coordinator
Scope: challenge clean claims and inspect omitted operational boundaries
Files/components inspected: existing audit artifacts, RLS, v1/v2 split, cache, manifests, tests
Entry points inspected: internal routes, legacy paths, direct DB role, cache files
Important callers/callees: deployment → role/RLS; request → fallback identity; manifest → active snapshot
Workflows traced: “atomic” activation, “stable” identities, “tenant isolated” SQL, “fresh” cache
Trust boundaries: claims versus runtime topology; comments versus write path
Attacker-controlled inputs: valid partial manifests and repeated requests
Invariants tested: claims were compared against concrete line-level writes
Findings: the largest missed issue is semantic mismatch between schema comments and implementation: stable identities and complete snapshots are asserted but not enforced; RLS tests use a special role absent from deployment evidence
Suspicions requiring validation: live RLS bypass and cache partial writes
Tests/commands executed: independent source pass; `go test ./...` pass
Coverage gaps: live deployment and full benchmark suite
Areas explicitly not reviewed: private systems
Recommended follow-up: make invariants executable in schema/tests and rerun live blind-spot review
