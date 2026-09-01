Skill: hostile-repository-auditor (implementation-assurance verification pass)
Agent: fork of the implementation-assurance session, 2026-08-29
Scope: services/execution-proxy only. Re-verify prior CONFIRMED findings against current code (business-logic-abuse F1/F2/F3, exploit-chain RCE, concurrency retry-reexecution, test-gap destructive/identity-mismatch coverage), close any narrow real test-gap, do not redesign authz architecture.

Files/components inspected:
- services/execution-proxy/cmd/server/main.go (full, 1267 lines)
- services/execution-proxy/cmd/server/main_test.go (full, 532 lines pre-change)
- packages/auth/middleware.go (ControlPlaneHandler, AuthenticatedHandler, RequireScope — full read)
- database/migrations/000001_initial.sql (operations table), 000014_tool_governance_persistence.sql

Entry points inspected: /health/live, /health/ready, /v1/version, /v1/plan, /v1/execute, /v1/proxy/execute (alias of handleExecute), /v1/results/{id}, /v1/results, /v1/proxy/restore(/{handle}), /v1/proxy/explain, /v1/rules (GET/POST). All routes except health probes sit behind `auth.ControlPlaneHandler(mux, auth.MustFromEnvironment())` (main.go:522). ControlPlaneHandler = AuthenticatedHandler (shared static `SCOS_INTERNAL_SERVICE_TOKEN` or HMAC claims token) AND a second shared static `X-SCOS-Control-Plane-Token` header equal to `SCOS_CONTROL_PLANE_TOKEN` (packages/auth/middleware.go:154-166). Neither check is scoped to a specific caller, tenant, or route (no `auth.RequireScope` used anywhere in this service — confirmed by grep; ControlPlaneHandler is the only gate, and it is identical for every mutating and read route).

Important callers/callees:
- handleExecute -> s.governance.AuthorizeAndPrepare (HTTP POST to tool-governance /v1/operations/prepare, main.go:144-152) -> executeCommand -> s.governance.Reconcile (POST /v1/operations/reconcile, main.go:154-158).
- executeCommand -> exec.CommandContext(ctx, "/bin/sh", "-c", command) (main.go:1182) — the literal `plan.ExecutedRequest` string, derived from the caller's `Command` field (rewritten only if AllowRewrite+eligible mode, otherwise passthrough), runs via a shell.

Workflows traced:
1. Caller -> POST /v1/execute with `{tenant_id, principal_id, command, operation_id, confirmed}` and `X-SCOS-Tenant-ID`/`X-SCOS-Principal-ID` headers -> validateExecutionRequest -> planCommand -> governance.AuthorizeAndPrepare -> executeCommand -> Reconcile -> persist result + raw artifact -> response.
2. Destructive-command path: classifyRisk keyword match -> effectiveMode defaults to forensic (raw passthrough, no rewrite) -> validateExecutionRequest requires `req.Confirmed == true` or rejects with 400 (main.go:555-557).
3. Identity path: validateExecutionRequest requires body `tenant_id`/`principal_id` to equal `X-SCOS-Tenant-ID`/`X-SCOS-Principal-ID` header values (main.go:544-550); both are attacker-supplied inputs, not derived from `auth.CanonicalClaims`.

Trust boundaries:
- External/agent caller -> execution-proxy: gated only by the two shared static secrets (internal service token + control-plane token), identical for every tenant and every caller in the mesh. No per-tenant or per-principal credential exists at this boundary.
- execution-proxy -> host OS: `/bin/sh -c <command>` with the caller's (possibly rewritten) command string. This is the RCE-relevant boundary.
- execution-proxy -> tool-governance: HTTP POST carrying `req.TenantID`/`req.PrincipalID` as supplied by the caller, not independently re-verified by execution-proxy.

Attacker-controlled inputs: `ExecuteRequest.Command` (shell command run verbatim), `TenantID`/`PrincipalID` (body and header, self-consistent by construction since attacker controls both), `Confirmed`, `AllowRewrite`, `RiskTolerance`, `Mode`, `OperationID`, `WorkingDir`.

Invariants tested:
- Destructive command without `confirmed:true` is rejected (400) — now via `TestHandleExecuteRejectsDestructiveCommandWithoutConfirmation`, added this pass.
- Body tenant/principal mismatching the authenticated headers is rejected (401) — now via `TestHandleExecuteRejectsBodyIdentityMismatchingAuthenticatedHeaders`, added this pass.
- Unprepared/unauthorized governance operation is rejected (403) — pre-existing (`TestExecuteRejectsGovernanceOperationMissingPreparation`, `TestExecuteRejectsDirectProxyRequestWithoutPreparedAuthorization`).

Findings:

1. [Confirmed, unchanged from hostile-business-logic-abuse-auditor.md F1] The destructive-command "confirmation gate" is not a distinct human-in-the-loop control — `Confirmed` is a caller-supplied boolean on the same JSON body the caller also controls (main.go:62, checked at :555-557). Nothing between the caller and this check requires a separate approval signal (no signed token, no second-party attestation). Any caller able to reach `/v1/execute` can simply set `"confirmed": true` in the same request. Verified against current code, not just the old report — code is unchanged in this respect since that audit. Out of scope to redesign per this pass's instructions; documented, not fixed.

2. [Confirmed, unchanged from hostile-exploit-chain-auditor.md] `/v1/execute` is reachable by any holder of the two shared static secrets (`SCOS_INTERNAL_SERVICE_TOKEN`/HMAC claims token + `SCOS_CONTROL_PLANE_TOKEN`), which are identical for every service and every tenant in the mesh (packages/auth/middleware.go:137-166). `validateExecutionRequest` (main.go:534-562) derives tenant/principal from `X-SCOS-Tenant-ID`/`X-SCOS-Principal-ID` request headers and the JSON body — both attacker-supplied — and only checks they agree with each other (main.go:544-550), never against `auth.CanonicalClaims`. A holder of the shared secrets can therefore execute an arbitrary shell command (`/bin/sh -c`, main.go:1182) under any tenant/principal identity of their choosing, laundered through tool-governance's own operation record under that same self-asserted identity. This is architecturally identical to what the exploit-chain report found; still present.

3. [Confirmed, unchanged from hostile-concurrency-auditor.md #1] `handleExecute` has no idempotency/dedup check on `operation_id` before calling `executeCommand` (main.go:688) — a retried request with the same `operation_id` re-runs the shell command a second time. `s.results[opID]` is only checked for other purposes (never as a "already executed" guard) before `executeCommand`. Confirmed unchanged in current code.

4. [Hardening opportunity, was a Test-gap finding, now closed] hostile-test-gap-auditor.md's two gaps ("no test proves destructive commands without confirmation are rejected" and "no test proves tenant/principal mismatch is rejected") are fixed this pass: `TestHandleExecuteRejectsDestructiveCommandWithoutConfirmation` and `TestHandleExecuteRejectsBodyIdentityMismatchingAuthenticatedHeaders` added to main_test.go, driven through the real `handleExecute` HTTP handler (not helper functions), and mutation-tested (see below).

5. [Non-issue] `/v1/rules` POST has no scope/role gate beyond ControlPlaneHandler, letting any control-plane caller add rewrite rules globally. Given finding 2 already establishes that any such caller can run arbitrary shell commands directly, this is not an incremental privilege escalation — noted for completeness only.

6. [Non-issue] execution-proxy does not itself read/write the `operations` table (database/migrations/000001_initial.sql:166) directly; that table is owned and mutated by tool-governance via the HTTP calls in `toolGovernanceClient`. execution-proxy's own persistent state (`results`/`history`/`rawMetadata`) is stored via `packages/service-state.Repository` in the generic `service_state` table, keyed by service name — consistent with the pattern used elsewhere in the repo (e.g. recovery-orchestrator's own tables, by contrast, are service-specific; execution-proxy's use of the shared `service_state` blob is an existing, unremarkable design choice, not a gap).

Suspicions requiring validation:
- Whether any deployed caller of `/v1/execute` other than `autonomous-control-plane` exists, and whether that caller (or any other shared-secret holder) is reachable by an external/lower-trust actor — this determines the real-world exploitability of findings 1/2 and was out of scope for this single-service pass (see hostile-exploit-chain-auditor.md for the fuller chain).
- Whether tool-governance's own `/v1/operations/prepare` independently re-derives tenant/principal from its own validated claims (would partially mitigate finding 2) — not re-checked this pass; tool-governance is a separate service outside this assignment.

Tests/commands executed:
- `go build ./...` and `go test ./...` (whole repo) — at the time of this pass, `services/security-gate` had pre-existing uncommitted changes (`git status` showed `main.go`/`main_test.go` modified before this fork started) that fail to compile (`undefined: DataClass`, `AuthToken`, `AuditRecord`, `SecretReference` in store.go) — unrelated to execution-proxy and out of this fork's scope per instructions to leave other services' dirty work alone.
- `go build $(go list ./... | grep -v services/security-gate)` — clean.
- `go test $(go list ./... | grep -v services/security-gate)` — clean, all pass (including `services/execution-proxy/...` with the two new tests); only pre-existing no-test-file notices for `api` and `packages/llmclient`.
- Mutation test 1: disabled the destructive-confirmation check (`if risk == "destructive" && !req.Confirmed` -> `if false && ...`), reran `TestHandleExecuteRejectsDestructiveCommandWithoutConfirmation` — failed as expected (403 governance error instead of the 400 the test wants, i.e., the request was no longer rejected at validation), then restored and reconfirmed the full suite passes.
- Mutation test 2: replaced the identity-mismatch check with `if false {}`, reran `TestHandleExecuteRejectsBodyIdentityMismatchingAuthenticatedHeaders` — failed as expected, then restored and reconfirmed the full suite passes. (First mutation attempt via `&&`-prefixing `false` did not actually disable the check due to `&&`/`||` operator precedence — caught this, corrected to a full `if false {}` replacement before treating the mutation test as valid.)

Coverage gaps:
- No test exercises `/v1/rules` POST access control (there is none to exercise beyond ControlPlaneHandler, per finding 5).
- No test exercises the concurrent-retry re-execution scenario from finding 3 (out of scope to fix here per this pass's instructions, since it's a pre-existing Confirmed finding needing an idempotency-guard design decision, not a narrow test-gap).

Areas explicitly not reviewed: tool-governance's own implementation (out of this service's scope), autonomous-control-plane's routing decisions that set `Confirmed`/`RiskTolerance` when calling execution-proxy (covered by the other implementation-assurance fork), mcp-gateway's outer boundary in front of this mesh.

Recommended follow-up:
- Findings 1 and 2 (confirmation-gate-is-not-real, shared-secret-only tenant assertion) are the same Critical/High items already tracked in hostile-business-logic-abuse-auditor.md and hostile-exploit-chain-auditor.md; they require a product/security design decision (e.g., a signed approval token from a distinct human-in-the-loop service, and/or deriving tenant/principal from `auth.CanonicalClaims` instead of caller-supplied headers) rather than a narrow code fix, and were intentionally not altered in this pass per its instructions.
- Finding 3 (retry re-execution) should get an idempotency guard: check `s.results[req.OperationID]` before calling `executeCommand` and return the cached result on a retry, mirroring the idempotency-key pattern already used on the `toolGovernanceClient.post` side (main.go:170).
