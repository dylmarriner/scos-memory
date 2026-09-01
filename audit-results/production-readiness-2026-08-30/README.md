# Production-readiness hostile audit — 2026-08-30

Agent: Codex, sequential hostile-audit coordinator. Existing worktree changes and prior audit artifacts were preserved and treated as in scope.

## Scope and method

The review covered the Go services and shared packages, new cloud-control-plane code, PostgreSQL migrations/policies, Docker Compose/native deployment, adapters, workers, CI, and existing audit artifacts. The 17 canonical hostile skills under `skills/hostile-*/SKILL.md` were read and applied sequentially; no independent-agent runtime was available, so independence is not claimed.

Commands and evidence are recorded in the specialist artifacts and `verification.md`. Static findings are not represented as live-runtime proof where PostgreSQL, Docker, TLS, or external identity infrastructure was unavailable.

## Executive result

**Not production-ready.** Two confirmed deployment blockers remain in the current worktree:

1. `deploy/compose/docker-compose.yml:735-736` publishes cloud-control-plane on `0.0.0.0` by default, unlike the explicitly bound internal/external services. This exposes a new customer/admin surface to every host interface unless an operator supplies a bind address.
2. `deploy/compose/docker-compose.yml:744-745` probes `https://127.0.0.1:8099/health/ready`, while `services/cloud-control-plane/cmd/server/main.go:103-108` defaults to `:8079`. The service therefore cannot become healthy under the default configuration.

Additional high-confidence risks requiring remediation before launch:

- Cloud session-authenticated mutation endpoints have no CSRF token/origin defense (`main.go:318-323`, `334-415`, `482-510`; `teams.go:31-224`). SameSite=Lax reduces common cross-site POST exposure but is not a complete production CSRF control.
- Team invitation/member-limit checks are raceable: the count is read and the member is inserted in separate operations without locking (`teams.go:122-137`, `170-180`). Concurrent valid requests can exceed `max_members`.
- RLS remains unverified against the shipped database role, and prior artifacts identify the absence of `FORCE ROW LEVEL SECURITY`; this audit did not claim that cross-tenant isolation is live-safe without a real cluster test.
- Prior confirmed findings remain relevant unless their referenced fix is independently reverified: human confirmation is classifier-derived in autonomous control-plane (`audit-results/hostile-implementation-assurance-autonomous-control-plane.md`), shell execution is an intentional high-impact capability, and several authoritative states remain in memory.

## Release gate

Do not release until the two deployment blockers are fixed and verified, then close or explicitly accept the CSRF, team-concurrency, live-RLS, durable-state, human-approval, and runtime-topology gaps listed in `findings.md`.

