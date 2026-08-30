# Specialist audit artifact

```text
Skill: hostile-repository-auditor (implementation-assurance verification pass)
Agent: hostile-implementation-assurance-agent-registry
Scope: services/agent-registry only. Verify prior hostile-* findings referencing this
  service are still accurate against current code, verify tenant/principal isolation,
  check for the auth.RequireScope test-gap regression pattern already found and fixed
  in services/recovery-orchestrator and services/backup-manager (extract newXMux +
  regression test), check migration-table consumption, and look for stubs/TODOs.

Files/components inspected:
  - services/agent-registry/cmd/server/main.go (full, 741 lines)
  - services/agent-registry/cmd/server/main_test.go (full, 218 lines)
  - database/migrations/000012_agent_registry_persistence.sql (full)
  - audit-results/hostile-authorization-auditor.md (prior finding referencing this
    service, used as the baseline claim to re-verify)

Entry points inspected:
  - GET /health/live, GET /health/ready (unauthenticated inside the mux, but the whole
    mux is wrapped by auth.AuthenticatedHandler in main() — see Trust boundaries)
  - GET /v1/agents, GET /v1/agents/{id} (handleAgents, handleAgentByID — read-only,
    tenant-scoped by claims)
  - POST /v1/agents/register (handleAgentRegister — self-service write, tenant pinned
    to caller's own claims.TenantID)
  - POST /v1/agents/heartbeat (handleAgentHeartbeat — self-service write, same pinning)
  - GET /v1/handoffs, GET /v1/handoffs/{id} (handleHandoffs, handleHandoffByID —
    read-only, tenant-scoped)
  - POST /v1/handoffs/initiate, POST /v1/handoffs/respond (self-service writes, same
    identity pinning as above)
  All ten routes are registered on one http.NewServeMux() built inline in main()
  (main.go:171-186) and wrapped once with auth.AuthenticatedHandler(mux,
  auth.MustFromEnvironment()) (main.go:192). No auth.RequireScope call exists anywhere
  in this service (confirmed by grep) — there is no elevated/admin-only mutating route
  in agent-registry's design; every mutating route acts only on the caller's own
  claims-derived tenant/principal, never on another tenant's data or a fleet-wide
  resource, so there is nothing here that plausibly requires a scope beyond "is
  authenticated as some tenant".

Important callers/callees:
  - resolveIdentity(r, bodyTenant, bodyPrincipal) (main.go:104-126) is the single choke
    point every handler calls before touching the database. It is claims-first: when
    auth.CanonicalClaims(ctx) succeeds, the body's tenant_id/principal_id (if present)
    must equal the claims' values or the request is rejected 403; if claims are absent
    but auth.Identity(ctx) is non-empty (i.e. a shared-token/non-claims caller passed
    auth.AuthenticatedHandler), the request is rejected 401 rather than falling back to
    trusting X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers. The header-only fallback path
    only activates when auth.Identity(ctx) == "", which cannot occur on a live request
    that has passed auth.AuthenticatedHandler (that middleware always sets an identity
    on success) — main_test.go:146-157 (TestResolveIdentityFromHeaders) exercises this
    fallback by calling resolveIdentity() directly with no auth context at all, which is
    a valid unit test of the fallback branch itself but is not, and does not claim to be,
    a test that the header fallback is reachable over the real HTTP path.
  - setTenantContext(ctx, tx, tenantID) (main.go:128-131) issues
    `SELECT set_config('scos.tenant_id', $1, true)` inside every transaction before any
    query, binding Postgres RLS (if configured on agent_registrations/agent_handoffs) to
    the same tenant value used in the Go-level WHERE clause — belt-and-suspenders
    tenant isolation at both the query and the session-config layer.
  - queryAgents / queryHandoffs (main.go:467-496, 688-718) always take a WHERE clause
    supplied by the calling handler, not by request input; every call site in this file
    passes a fixed literal ("tenant_id=$1" or "tenant_id=$1 AND id=$2") with
    identity.TenantID (never request-body/header-only values) as the parameter — no SQL
    injection surface, no cross-tenant read path.

Workflows traced:
  1. Authenticated caller with valid claims -> resolveIdentity -> tenant/principal
     pinned to claims -> BeginTx -> setTenantContext(RLS session var) -> tenant-scoped
     SQL -> Commit. Verified for all six data-touching handlers.
  2. Attempted cross-tenant write: caller with claims{tenant: A} POSTs a body with
     tenant_id: B to /v1/agents/register or /v1/handoffs/initiate -> resolveIdentity's
     `bodyTenant != "" && bodyTenant != claims.TenantID` check (main.go:106) rejects
     with 403 before any DB call. No code path bypasses this for any of the four
     mutating handlers (register, heartbeat, handoff/initiate, handoff/respond) — each
     calls resolveIdentity with the request body's tenant/principal before any DB write.
  3. Agent staleness reporting: agentEffectiveStatus (main.go:450-465) recomputes
     "active" vs "inactive" from real elapsed wall-clock time against last_seen_at
     rather than echoing back a persisted status forever; a non-"active" persisted
     status (e.g. operator-set "degraded") passes through unchanged. Regression-tested
     in TestAgentEffectiveStatusReflectsRealStaleness (main_test.go:164-188), including
     the fail-open case for an unparseable timestamp.

Trust boundaries:
  - auth.AuthenticatedHandler (packages/auth/middleware.go) is the sole authentication
    gate in front of the entire mux; a caller must present a valid bearer token to reach
    any route, including /health/live and /health/ready (unauthenticated liveness checks
    are effectively unreachable without a token, which is a minor operational
    inconvenience, not a security gap, and matches the sibling services audited
    elsewhere in this repo).
  - Inside that boundary, resolveIdentity is the tenant boundary: it is the only place
    in the service that decides "which tenant does this request act as," and it is
    claims-derived, not attacker-header-derived, on every live request.

Attacker-controlled inputs:
  - JSON request bodies for AgentRegistration and HandoffPacket (name, version,
    maintainer, capabilities/inputs/outputs lists, tool/memory permission lists,
    max_classification, model_provider, cost_policy, health_endpoint, objective,
    constraints, namespaces, memory/artifact references, acceptance criteria,
    expires_at) — all stored verbatim as jsonb/text with no downstream code execution
    or template interpolation found in this service. No path traversal, command
    injection, or SQL injection sink located for any of these fields.
  - Path segment after /v1/agents/ and /v1/handoffs/ — validated with isUUID()
    (main.go:133-150) before use in any query; non-UUID input rejected 400 before
    reaching the database (main_test.go:85-93, 117-125 cover this).
  - bodyTenant/bodyPrincipal on all four mutating endpoints — constrained to equal the
    authenticated caller's own claims as described above; cannot be used to act as
    another tenant/principal.

Invariants tested:
  - A caller cannot register an agent, send a heartbeat, initiate a handoff, or respond
    to a handoff under any tenant_id/principal_id other than their own authenticated
    identity (resolveIdentity mismatch -> 403).
  - A caller can only read the agent/handoff rows belonging to their own tenant
    (queryAgents/queryHandoffs always scoped by identity.TenantID).
  - Agent status staleness is derived from real elapsed time, not fabricated/echoed
    indefinitely (agentEffectiveStatus).
  - Path IDs must be well-formed UUIDs before reaching SQL.

Findings:
  - [Non-issue] The auth.RequireScope test-gap regression class already found and fixed
    in services/recovery-orchestrator and services/backup-manager (mutating routes
    scope-gated in main() but only exercised via direct handler calls in tests, never
    through the real mux) does not apply to agent-registry: this service registers zero
    auth.RequireScope-wrapped routes (confirmed by grep across the package). Its
    mutating routes are intentionally self-service-only (a caller can only ever write
    its own tenant's rows), so there is no elevated/admin capability here for a missing
    scope gate to hide. Extracting a newAgentRegistryMux(s) purely to mirror the pattern
    would add no security coverage, since there is no RequireScope call to regress.
  - [Non-issue] The prior hostile-authorization-auditor.md finding that agent-registry's
    resolveIdentity() is "confirmed NOT vulnerable via the real HTTP path" (its header-
    fallback branch is reachable only when a test calls resolveIdentity() directly,
    never through auth.AuthenticatedHandler) is re-verified true against the current
    code: main.go:192 still wraps the entire mux in auth.AuthenticatedHandler, and
    resolveIdentity's header-fallback branch (main.go:114-124) is gated behind
    `auth.Identity(r.Context()) != ""` returning false, which auth.AuthenticatedHandler
    never produces on a successfully authenticated request.
  - [Hardening opportunity] handleAgentRegister, handleHandoffInitiate, and
    handleHandoffRespond decode request bodies with a plain json.NewDecoder(r.Body) —
    no http.MaxBytesReader cap and no DisallowUnknownFields(), unlike the pattern used
    in services/recovery-orchestrator (bounded to 64<<10 bytes, unknown fields
    rejected). An authenticated caller (of any tenant) could send an arbitrarily large
    body to these three endpoints; since capabilities/inputs/outputs/constraints/etc.
    are all unbounded []string fields stored as jsonb with no length caps either, this
    is a resource-exhaustion/DB-bloat vector scoped to the caller's own tenant rather
    than a cross-tenant issue. Low severity given the self-service trust model, but
    worth aligning with the recovery-orchestrator/backup-manager convention.
  - [Non-issue] handleAgents, handleAgentByID, handleHandoffs, and handleHandoffByID
    perform no r.Method check (unlike the four mutating handlers, which all reject
    non-POST with 405). Since these four are read-only SELECT-only code paths
    regardless of HTTP verb, this is a REST-semantics laxity, not a security or
    correctness defect.
  - [Non-issue] All jsonb/text columns declared in
    database/migrations/000012_agent_registry_persistence.sql for both
    agent_registrations and agent_handoffs are read and written by queryAgents (main.go
    SELECT list, main.go:469-471) / the INSERT in handleAgentRegister (main.go:307-317)
    and queryHandoffs (main.go:690-693) / the INSERT in handleHandoffInitiate
    (main.go:600-610) respectively — no declared-but-unused column, no
    consumed-but-undeclared column.

Suspicions requiring validation:
  - Whether database/policies/ actually defines RLS policies on agent_registrations and
    agent_handoffs that key off scos.tenant_id (this pass confirmed the Go code sets
    that session var via setTenantContext before every query, matching the convention
    used elsewhere in the repo, but did not independently re-verify the RLS policy
    definitions exist and are enabled on these two specific tables — that is
    database/policies/ territory, out of this service-scoped pass).
  - Whether any peer service (e.g. tool-governance, execution-proxy) reads
    agent_registrations/agent_handoffs directly via a shared DB connection rather than
    through this service's HTTP API, which would bypass resolveIdentity's tenant
    pinning entirely. No such direct DB access was found via grep for these two table
    names outside services/agent-registry and database/migrations, but a repo-wide
    exhaustive search for indirect ORM/ad hoc SQL access was not performed as part of
    this pass.

Tests/commands executed:
  - `grep -rn "auth.RequireScope" services/agent-registry` -> 0 matches (confirms no
    scope-gated routes exist to regress).
  - `grep -n "CREATE TABLE" database/migrations/000012_agent_registry_persistence.sql`
    and manual column-by-column comparison against main.go's SELECT/INSERT column lists.
  - `grep -rniE "TODO|FIXME|not implemented|panic\(|stub" services/agent-registry
    --include=*.go` -> 0 matches.
  - `go build ./...` from repo root -> clean (see command executed in the coordinating
    session's final consolidation).
  - `go test ./services/agent-registry/...` -> all existing tests pass unchanged; no
    code was modified in this service during this pass.

Coverage gaps:
  - No live Postgres instance was started; RLS-policy-level enforcement was not
    independently exercised end-to-end, only inferred from setTenantContext's presence
    and prior confirmation in audit-results/hostile-persistence-auditor.md-style passes
    for sibling services.
  - Load/DoS behavior of the unbounded-body-size handlers noted above was not measured.

Areas explicitly not reviewed:
  - services/autonomous-control-plane, services/execution-proxy, services/security-gate
    — covered separately in sibling implementation-assurance passes
    (audit-results/hostile-implementation-assurance-autonomous-control-plane.md,
    -execution-proxy.md, -security-gate.md).
  - database/policies/ RLS definitions themselves (only their expected use from Go code
    was checked here).

Recommended follow-up:
  - Optional hardening: add http.MaxBytesReader + dec.DisallowUnknownFields() to
    handleAgentRegister, handleHandoffInitiate, and handleHandoffRespond, matching the
    convention already used in services/recovery-orchestrator, to close the
    same-tenant resource-exhaustion vector noted above. Not fixed in this pass since it
    is a hardening opportunity, not a regression of a previously-confirmed finding, and
    the task scope prioritized verifying/fixing the RequireScope test-gap class, which
    does not exist in this service.
```
