Skill: hostile-repository-auditor (implementation-assurance pass)
Agent: Claude Sonnet 5 (fork)
Scope: services/autonomous-control-plane only. Verification pass re-checking prior findings from hostile-control-flow-auditor.md, hostile-business-logic-abuse-auditor.md, and hostile-persistence-auditor.md against current code; no redesign of the confirmation-forwarding or permission-checking architecture attempted (explicitly out of scope per task).

Files/components inspected:
- services/autonomous-control-plane/cmd/server/main.go (1275 lines, full)
- services/autonomous-control-plane/cmd/server/classifier.go (full)
- services/autonomous-control-plane/cmd/server/classifier_test.go (full)
- services/autonomous-control-plane/cmd/server/capabilities.go (full)
- services/autonomous-control-plane/cmd/server/persistence.go (full)
- services/autonomous-control-plane/cmd/server/main_test.go (function inventory + spot read)
- services/autonomous-control-plane/cmd/server/readiness_test.go (full, 9 lines)
- database/migrations/000003_control_plane.sql, 000004_autonomous_control_plane.sql, 000006_reconcile_routing_decisions.sql

Entry points inspected:
- POST /v1/control/route, /v1/control/ingest, /v1/triggers -> plane.handleIngest (main.go:1245-1251)
- POST /v1/control/replay -> plane.handleReplay
- POST /v1/control/recover -> plane.handleRecover
- GET /v1/control/status, /v1/control/status/{id} -> plane.handleStatus
- GET /v1/control/decisions, /v1/control/decisions/{id} -> decisionHandler (main.go:1214-1244)
- GET /health/live, /health/ready (unauthenticated by design, matches sibling services)
- All routes sit behind one blanket `auth.AuthenticatedHandler(mux, ...)` (main.go:1258); no `auth.RequireScope` is used anywhere in this service (grep-confirmed, 0 hits).

Important callers/callees:
- executeRoute() (main.go:390-537) drives the per-stage plan built by buildRoute()/classifyIntent(); it calls memory-gateway, code-intelligence, context-optimisation, tool-governance, adaptive-execution-proxy, agent-router, and the turn-coordinator (commitReply) over HTTP via callService()/dispatchAgent()/memoryGatewayCommitter.
- selectCapabilities() (capabilities.go:267-310) runs against the in-process `routingRegistry` (newPlatformRegistry(), capabilities.go:20,234-244), not any database-backed or externally-verified capability source.
- grantedPermissions() (capabilities.go:250-262) is the sole source of "permissions" fed into capability selection.

Workflows traced:
1. Caller -> /v1/control/ingest -> authenticatedIdentity() (claims-derived tenant/actor) -> buildRoute() -> classifyIntent() -> selectCapabilities()/grantedPermissions() -> persistRoute() -> executeRoute() -> per-stage dispatch, including the "adaptive-execution-proxy" stage's confirmed flag.
2. Recovery: /v1/control/recover -> beginRecovery() -> `SELECT ... FOR UPDATE` on orchestration_runs, transitions running/interrupted -> recovering.
3. Persistence: persistRouteDB() (main.go:651-696) writes orchestration_runs + routing_decisions under `set_config('scos.tenant_id'...)`/`set_config('scos.principal_id'...)` for RLS.

Trust boundaries:
- HTTP boundary: caller -> autonomous-control-plane, gated by auth.AuthenticatedHandler (bearer token) only; no per-route scope check.
- autonomous-control-plane -> downstream services (execution-proxy, tool-governance, memory-gateway, etc.): each call carries the caller's bearer token (auth.BearerToken(ctx)) plus X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers derived from the authenticated identity (main.go:344-351), not from client-controlled RouteRequest fields directly — TenantID/ActorID on the outgoing call are `req.TenantID`/`req.ActorID`, which were already overwritten with claims values at the top of handleIngest (main.go:891: `req.TenantID, req.ActorID = tenantID, actorID`). Good: downstream tenant/principal headers are claims-derived, not attacker-supplied.
- The `confirmed` flag sent to adaptive-execution-proxy (main.go:465) is derived entirely from this service's own `decision.ConfirmationRequired`, itself derived from `classifyIntent()`'s heuristic labels and `req.RiskClass` (client-supplied, main.go:1049-1051, 152-155) — see Findings AUTOCP-1.

Attacker-controlled inputs:
- RouteRequest.RiskClass (client body field) directly influences `confirmation` (main.go:1071) and, via classifyIntent's "explicit_risk_class" observation (classifier.go:152-155), the `high_impact_action` label.
- RouteRequest.Input (free text) drives phrase-based classification (classifier.go:157-172), which is weaker evidence (confidencePhrase=0.62) than explicit signals but still contributes admitted labels at the 0.60 threshold.
- RouteRequest.ProposedTool becomes the literal `command` field forwarded to adaptive-execution-proxy's /v1/proxy/execute (main.go:464), unchanged from the client body.

Invariants tested (spot-checks against current code, not new tests written):
- Tenant/principal never taken from client body when claims are present: verified authenticatedIdentity() (main.go:277-298) rejects a body tenant/actor that disagrees with claims (403) and otherwise uses claims values exclusively.
- routing_decisions/orchestration_runs writes always run under `set_config('scos.tenant_id', $1, true)` / `set_config('scos.principal_id', $1, true)` before any INSERT/SELECT — confirmed at every one of persistRouteDB, decisionForInteraction, beginRecovery, lifecycleDB, persistLifecycleDB.
- Production cannot silently fall back to an unauthenticated in-memory store: main() hard-fails (`os.Exit(1)`) if DATABASE_URL is unset (main.go:1188-1191) and if SCOS_MEMORY_GATEWAY_URL is unset (main.go:1192-1196) — the localStateStore path is genuinely test-only, matching its own comment (main.go:198-202).

Findings:

1. [High-confidence, unchanged from hostile-business-logic-abuse-auditor.md F3] The "adaptive-execution-proxy" pipeline stage sends `"confirmed": !decision.ConfirmationRequired` to execution-proxy automatically (main.go:465), with no separate human-in-the-loop signal. `decision.ConfirmationRequired` is computed once in buildRoute() (main.go:1071: `risk == "high" || risk == "critical" || class.has("high_impact_action")`) from client-supplied `RouteRequest.RiskClass`/`Input`/`OperationType`, all attacker-controlled. A caller can therefore set `risk_class` to anything other than "high"/"critical" and phrase `input` to avoid the `high_impact_action` phrase list (classifier.go:107) to make `ConfirmationRequired=false`, at which point this service auto-forwards `confirmed: true` to execution-proxy on the caller's behalf. This reproduces the exploit-chain-auditor.md and business-logic-abuse-auditor.md finding unchanged; not touched in this pass because closing it is a product-security architecture decision (a real "confirmed by a human" signal must originate outside the classifier), not a narrow test-gap.

2. [High-confidence, unchanged from hostile-control-flow-auditor.md] `grantedPermissions()` (capabilities.go:250-262) is the only source of "permissions" used by `selectCapabilities()`'s policy check (capabilities.go:279 `GrantedPermissions: granted`). It is derived purely from this service's own classifier output (`c.has("tool_operation")`, etc.) — there is no call anywhere in `services/autonomous-control-plane` to security-gate's permission-check endpoint or any other authoritative source (grep-confirmed: 0 hits for "permission" outside this self-referential mechanism). This means capability selection's "policy" is self-attested: any caller who can get the classifier to admit a label (via explicit fields or phrase-matching, all attacker-influenced per classifier.go:112-172) is granted the corresponding synthetic permission for selection purposes. Downstream services (tool-governance, execution-proxy) may or may not independently re-check; that's outside this service's scope but this service's own gate provides no real authorization value.

3. [Hardening opportunity] Migration 000004_autonomous_control_plane.sql declares `autonomous_triggers` and `capability_registry` tables. Neither is referenced anywhere in services/autonomous-control-plane (grep-confirmed, 0 hits for both `autonomous_triggers` and `capability_registry` in *.go). The `/v1/triggers` HTTP route (main.go:1251) exists and sounds like it should consult `autonomous_triggers`, but it is simply aliased to the same `plane.handleIngest` as `/v1/control/ingest` — it does not look up trigger policy, event_type, or handler_capability_key at all. `capability_registry` (a DB-backed capability catalog with `generation`, `health_state`, `quality_score` etc.) is fully superseded in practice by the hardcoded in-process `routingRegistry` (capabilities.go:20, `newPlatformRegistry()`). Both tables are dead schema from this service's perspective — either wire them up or drop them; a reader of `/v1/triggers` or `capability_registry` would reasonably but wrongly assume they do something.

4. [Non-issue] Migration column drift (routing_decisions): 000004's `CREATE TABLE IF NOT EXISTS routing_decisions (...)` is a no-op because 000003 already created the table with a different column set; 000006 reconciles this explicitly by ALTERing only `orchestration_run_id`/`decision_id` onto the 000003 table. Verified the code (persistRouteDB main.go:686, decisionForInteraction main.go:727-740) references only columns that actually exist under this reconciliation (`resolved_scope`, `candidate_capabilities`, `actual_outcome` from 000003, plus the two ALTERed columns) — it never references 000004-only column names like `confirmation_required`, `outcome`, or `context_capsule_id`, which were never created. No drift bug. This matches and confirms hostile-persistence-auditor.md's prior conclusion; re-verified against current code rather than re-asserted.

5. [Non-issue] No `auth.RequireScope`-style test gap applies to this service. Unlike recovery-orchestrator/backup-manager, every mutating route here (`/v1/control/ingest`, `/route`, `/triggers`, `/recover`, `/replay`) operates strictly on the caller's own claims-derived tenant/principal (authenticatedIdentity() rejects any mismatch) — there is no operator/admin action that acts on someone else's behalf, so there is no missing authorization boundary for a mux-level regression test to protect. The absence of RequireScope here is architecturally consistent, not a gap.

Suspicions requiring validation:
- Whether tool-governance and/or execution-proxy independently re-validate permissions/confirmation when called from autonomous-control-plane (would mitigate findings 1-2 if so) — out of this service's scope; per hostile-control-flow-auditor.md, execution-proxy's own confirmation check is the one actually bypassed by finding 1's forwarded `confirmed: true`, so the two findings compound rather than one covering for the other.
- Whether `autonomous_triggers`/`capability_registry` are consumed by some other, not-yet-audited service (e.g. an operator console) rather than being wholly dead — grep only covered this service's Go source.

Tests/commands executed:
- `go build ./...` from repo root — clean, exit 0.
- `go test ./services/autonomous-control-plane/...` — pass (cached, all prior tests green; no new tests added this pass).
- `grep -rn "permission" services/autonomous-control-plane --include=*.go -i` — only self-referential grantedPermissions/RequiredPermissions matches, confirming finding 2.
- `grep -rn "autonomous_triggers\|capability_registry\b" services/autonomous-control-plane --include=*.go` — 0 matches, confirming finding 3.
- `grep -n "TODO\|FIXME\|not implemented\|stub" -ri services/autonomous-control-plane --include=*.go` — 0 matches.

Coverage gaps:
- No test in main_test.go exercises the real `http.ServeMux` built in `main()` (tests call handler functions like `plane.handleIngest` directly). Not flagged as a security gap here (finding 5: no scope gate exists to bypass), but it does mean a future accidental change to route wiring in `main()` (e.g. a typo that maps `/v1/triggers` to the wrong handler, or an accidental scope requirement that would 403 legitimate self-service callers) would not be caught by any test in this package. Recommend, as a low-priority hardening item, extracting mux construction into a `newControlPlaneMux(*controlPlane) http.Handler` purely for route-wiring regression coverage, independent of any auth-gap concern.

Areas explicitly not reviewed:
- execution-proxy, tool-governance, security-gate internals (separate assigned scopes).
- Whether any downstream service independently mitigates findings 1-2.
- Load/concurrency behavior of executeRoute()'s per-stage HTTP calls (covered by hostile-concurrency-auditor.md for the execution-proxy side; not re-verified here for autonomous-control-plane).
- classifier_test.go and capabilities-related unit tests were not individually re-run beyond the package-level `go test`; no new classifier edge cases were probed.

Recommended follow-up:
- Product decision needed for findings 1-2: introduce a confirmation signal that autonomous-control-plane cannot compute from the same client-controlled input it uses for classification (e.g., a separate confirmation token/step that only a human-facing UI can produce), and/or have execution-proxy/tool-governance call security-gate's permission-check directly rather than trusting the caller's self-reported granted-permissions list.
- Either wire up `autonomous_triggers`/`capability_registry` or remove them from the schema to stop implying functionality that doesn't exist.
- Low-priority: extract mux construction to a testable function for route-wiring regression coverage (not a security fix, a correctness one).
