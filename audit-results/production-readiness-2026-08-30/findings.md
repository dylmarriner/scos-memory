# Findings

## PR-001 — Cloud control plane is published on all host interfaces

- Severity: High; status: Confirmed from deployment configuration.
- Evidence: `deploy/compose/docker-compose.yml:727-736` defines cloud-control-plane with `ports: - "${SCOS_CLOUD_PORT:-8079}:8079"`; no host address is supplied. Docker publishes to all interfaces. `.env.example:107` supplies only `SCOS_CLOUD_HTTP_ADDR=:8079`.
- Reachability: Any network client able to reach the Docker host can reach the cloud portal/API. The application has customer authentication for most API actions, but this is still an unintended exposure of signup/login/admin/MCP ingress and increases attack surface.
- Impact: Internet/LAN exposure, brute-force and application-layer attack surface, and possible admin/MCP abuse if an application-layer defect is found.
- Fix: Bind through an explicit `SCOS_CLOUD_BIND_ADDRESS` with a safe loopback default, or an explicitly documented ingress/Tailscale address. Add a deployment test asserting no wildcard publication.
- Falsification attempted: Compared all compose port mappings; mcp-gateway and memory-intelligence use explicit Tailscale binding, while cloud-control-plane does not.

## PR-002 — Cloud health check targets a different port than the service

- Severity: High; status: Confirmed.
- Evidence: `services/cloud-control-plane/cmd/server/main.go:103-108` defaults listener to `:8079`; `deploy/compose/docker-compose.yml:744-745` probes port `8099`.
- Impact: Default Compose deployment reports cloud-control-plane unhealthy and any health-gated consumer cannot rely on readiness. This is a release/operations blocker.
- Fix: Probe `8079` or make the healthcheck use the configured listener port consistently; add a compose/static consistency test.

## PR-003 — Cookie-authenticated mutations lack CSRF protection

- Severity: Medium/High depending on browser deployment; status: High-confidence.
- Evidence: Session cookie is issued at `main.go:303`; mutations use `current()` cookie auth at `main.go:318-323`, `334-415`, `482-510` and `teams.go:31-224`. No CSRF token, Origin/Referer validation, or explicit anti-CSRF middleware exists. `portal.go` sends cookie-authenticated POST/PATCH requests.
- Impact: A browser session can be induced to perform logout, API-key creation/revocation, profile changes, team creation/invites/acceptance, and other state changes if deployment/browser conditions bypass SameSite protection. SameSite=Lax is defense in depth, not a complete CSRF proof.
- Fix: Add a server-generated CSRF token bound to the session and require it on every cookie-authenticated state-changing request; also validate Origin for HTTPS deployments. Test missing, mismatched, and cross-origin cases.

## PR-004 — Team member limit is not concurrency-safe

- Severity: Medium; status: High-confidence.
- Evidence: `teams.go:122-129` reads member count, then generates/inserts invitation at `131-137`; `acceptInvite` reads count at `170-174`, then inserts membership at `177-180`. Neither count read locks the team nor is there a database invariant enforcing the cap.
- Impact: Concurrent valid invitations/acceptances can exceed the advertised `max_members` limit; duplicate requests can create operational and billing inconsistency.
- Fix: Lock the team row (`FOR UPDATE`) in the transaction before count and insert, and re-check invitation state under lock. Add a concurrent integration test.

## PR-005 — Live tenant isolation remains unproven and prior RLS evidence is adverse

- Severity: Critical pending environment confirmation; status: Plausible-to-high-confidence deployment risk.
- Evidence: Prior `audit-results/hostile-persistence-auditor.md` and `hostile-error-path-auditor.md` identify that RLS policies are not forced and the shipped connection role may own/bypass protected tables. `database/integration/rls_test.go` creates a special `NOBYPASSRLS` fixture role; it does not prove the Compose/native runtime role.
- Impact: Cross-tenant read/write if the application connection role bypasses RLS or if tautological policies remain. This must be tested against an exact production-like role, not inferred from policy source.
- Fix/verification: Create a non-owner `NOBYPASSRLS` app role, grant only required privileges, apply `FORCE ROW LEVEL SECURITY` where appropriate, and run the integration suite through the shipped DSN. Until then, treat tenant isolation as a release blocker for multi-tenant operation.

## PR-006 — Durable authority and human approval gaps remain

- Severity: High; status: Confirmed by prior current-tree artifacts.
- Evidence: `audit-results/hostile-implementation-assurance-autonomous-control-plane.md:37-53` records that `confirmed` is derived from caller-controlled risk/input classification and that synthetic permissions are classifier-derived rather than an authoritative security-gate decision. `audit-results/hostile-implementation-assurance-execution-proxy.md:66-67` records retry re-execution risk and the intentional shell capability.
- Impact: A caller can potentially steer a valid request through an apparently approved/high-impact path without a distinct human approval artifact; retries can repeat side effects.
- Fix: Require a signed, short-lived approval token from a distinct authority bound to operation, tenant, principal, command digest, and expiry; make execution idempotent at the effect boundary; constrain shell/workdir capability by explicit policy.

## PR-007 — In-memory authoritative state is not restart-safe

- Severity: High operational/correctness risk; status: Confirmed by prior artifacts.
- Evidence: Prior `audit-results/from-scos-memory/final-report.md` and specialist artifacts document in-memory governance/recovery state and loss on restart.
- Impact: Restart or rescheduling can lose authorization, operation, recovery, and audit state, causing inconsistent or unverifiable production behavior.
- Fix: Persist authoritative state and transitions transactionally, then test crash/restart/replay recovery.

## PR-008 — Agent bridges previously disabled TLS verification and used identity placeholders

- Severity: High; status: Confirmed before fix, remediated in this change.
- Evidence: the shared, Codex, and OpenCode bridge copies set `CERT_NONE` and supplied fixed tenant/principal defaults; Hermes had the same insecure fallback. This allowed interception or accidental cross-tenant attribution.
- Fix applied: all four bridges now require bearer token, tenant, and principal values, verify the configured CA/system trust by default, and allow certificate skipping only through explicit `SCOS_TLS_INSECURE_SKIP_VERIFY=1`. Placeholder identities were removed.
- Residual risk: the explicit development escape hatch must be prohibited by production deployment policy and the installed bridge copies must be kept synchronized.
