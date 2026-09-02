# Specialist audit artifact

Skill: hostile-configuration-auditor
Agent: Codex coordinator
Scope: env defaults, Compose, service URLs, database security
Files/components inspected: `.env.example`, `deploy/compose`, auth/database config, Dockerfiles
Entry points inspected: service startup and dependency URL resolution
Important callers/callees: env → server setup → database/TLS/auth
Workflows traced: local Compose startup; code store readiness; internal HTTP calls
Trust boundaries: deployment operator and network topology
Attacker-controlled inputs: environment/config and mounted repository path
Invariants tested: missing key/password fail-closed claims; TLS URL validation
Findings: RLS runtime role/ FORCE RLS absent from deployment evidence; service URL defaults are trusted configuration; broad host mounts and allowed-root behavior require live deployment verification
Suspicions requiring validation: exact Compose mount exposure in current worktree
Tests/commands executed: config/source review; Go suite pass
Coverage gaps: `docker compose config`, live startup, container scan
Areas explicitly not reviewed: host firewall
Recommended follow-up: deployment manifest parity and least-privilege DB role bootstrap
