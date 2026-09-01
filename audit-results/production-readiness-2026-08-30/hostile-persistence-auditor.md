# Specialist audit artifact

Agent: Codex sequential specialist pass
Scope: Repository-wide review focused on this specialist.
Files/components inspected: Go services/packages, new cloud-control-plane handlers and migrations 000021-000023, Compose/native deployment, CI, workers/adapters, and prior audit artifacts as applicable.
Entry points inspected: All Go listeners were enumerated; cloud routes /api/auth/*, /api/*, /api/admin/*, /api/teams/*, and /mcp were traced.
Callers/callees and workflows: External HTTP/MCP/cookie/API-key/config/DB inputs were traced to handlers, SQL, internal HTTP, subprocess, files, queues, and recovery paths.
Trust boundaries: network to service, browser to cookie API, API key to signed internal claims, DB/RLS to workers, config to deployment, and service to subprocess.
Attacker-controlled inputs: paths, methods, headers, cookies, JSON, query values, API keys, invitation tokens, persisted rows, environment values, external responses, replay and concurrency.
Invariants tested: auth before privilege; tenant/object binding; ordered/idempotent state; concurrency-safe limits; TLS/secrets/exposure fail closed; rollback and durable isolation.
Findings: Confirmed PR-001 (cloud wildcard port publication) and PR-002 (healthcheck probes 8099 while listener defaults 8079). High-confidence PR-003 CSRF and PR-004 team-limit race. Prior PR-005-007 risks remain unresolved as described in findings.md.
Suspicions requiring validation: live PostgreSQL role/RLS behavior, Docker/TLS topology, browser origins, queue semantics, and external identity/key provisioning.
Tests/commands executed: git inventory/status, rg route/sink/config searches, numbered source/migration/Compose inspection, and prior-artifact reconciliation. Full execution results are in verification.md.
Coverage gaps: no live database, Docker deployment, browser, queue, external identity provider, or production ingress.
Areas explicitly not reviewed: external systems outside this repository and unrelated dirty files beyond release impact.
Recommended follow-up: fix PR-001/002, then run live RLS, browser CSRF, concurrent team, approval/idempotency, restart, and deployment tests.

