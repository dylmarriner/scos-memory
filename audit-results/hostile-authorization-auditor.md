# Specialist audit artifact

```text
Skill: hostile-authorization-auditor
Agent: hostile-authorization-auditor-workflow-agent
Scope: Re-verify and extend the shared SCOS_INTERNAL_SERVICE_TOKEN + client-supplied
  X-SCOS-Tenant-ID/X-SCOS-Principal-ID identity-spoofing pattern across
  services/retrieval-orchestrator, services/agent-registry, services/graphify-provider,
  services/code-intelligence, services/context-optimisation, services/observability,
  services/backup-manager, services/recovery-orchestrator. Background-only: mcp-gateway,
  memory-gateway, memory-intelligence (already covered elsewhere; independently
  re-verified where they intersect the trace, not restated as new).

Files/components inspected:
  - packages/auth/middleware.go (AuthenticatedHandler, ControlPlaneHandler, validateClaims,
    CanonicalClaims, Identity) — the shared trust primitive every service below is built on.
  - services/retrieval-orchestrator/cmd/server/main.go (handleRetrieve and all DB query
    helpers, lines ~34, 174-201, 320-950)
  - services/agent-registry/cmd/server/main.go (resolveIdentity 91-126, handler wiring 172-192)
    and main_test.go (138-151)
  - services/code-intelligence/cmd/server/main.go (requireTenant 419-426, handleRepos
    488-551, handleRepoByID 553+, and all 7 other requireTenant call sites: 491, 554, 581,
    730, 1089, 1202, 1314; server wiring 390-416)
  - services/graphify-provider/cmd/server/graphify.go (postJSON 82-110) and
    services/graphify-provider/cmd/server/main.go (tenantID/principalID sourcing, 77, 120)
  - services/context-optimisation/cmd/server/main.go (handleContextEvaluate 242-279,
    handleContextRestore 284-315, validSourceRef 319-328, SourceStore/fileSourceStore
    190-233, handleInputOptimise 338-426, server wiring 179)
  - services/observability/cmd/server/main.go (handleMetrics/handleMetricsQuery 483-567,
    handleTraces/handleTraceIngest/handleTraceByID 571-653, server wiring 354, struct
    defs with optional Tenant field 41, 60, 75)
  - services/backup-manager/cmd/server/main.go (handler inventory 139-800, server wiring 245)
  - services/recovery-orchestrator/cmd/server/main.go (handler inventory, server wiring 121)
  - services/mcp-gateway/cmd/server/main.go (resolveIdentity 794-807, copyIdentityHeaders
    780-792, retrieval-orchestrator call site 523-620) — inspected only to trace the
    exploit chain into retrieval-orchestrator, not as primary scope.

Entry points inspected:
  - POST /v1/retrieve on retrieval-orchestrator (internal, ControlPlaneHandler-gated)
  - POST/GET /v1/repos, /v1/repos/{id}, /v1/index, /v1/query, /v1/context, /v1/impact,
    /v1/adapters on code-intelligence (ControlPlaneHandler-gated)
  - /v1/agents*, /v1/handoffs* on agent-registry (AuthenticatedHandler-gated)
  - /v1/context/evaluate, /v1/context/restore, /v1/context/optimize/input,
    /v1/context/optimize/output on context-optimisation (ControlPlaneHandler-gated)
  - /v1/metrics, /v1/metrics/query, /v1/traces, /v1/traces/{id}, /v1/logs on observability
    (AuthenticatedHandler-gated)
  - /v1/backups*, /v1/restore* on backup-manager (AuthenticatedHandler-gated)
  - recovery-orchestrator handlers (AuthenticatedHandler-gated)
  - mcp-gateway tool-call path that forwards into retrieval-orchestrator (for chain tracing only)

Important callers/callees:
  - auth.AuthenticatedHandler / auth.ControlPlaneHandler → auth.CanonicalClaims /
    auth.Identity → each service's own identity-derivation function (or lack thereof)
  - mcp-gateway resolveIdentity() → copyIdentityHeaders() → retrieval-orchestrator
    handleRetrieve() → Postgres queries parameterized directly by req.TenantID
  - agent-registry resolveIdentity() is the only one of the eight in-scope services that
    consults auth.CanonicalClaims and explicitly rejects a non-claims (shared-token)
    caller that has no tenant header context ("authenticated token has no tenant and
    principal claims", main.go:112) — used as the comparison baseline below.

Workflows traced:
  1. External/internal caller → mcp-gateway (shared token, no JWT) → sets
     X-SCOS-Tenant-ID to an arbitrary value → mcp-gateway resolveIdentity() falls back
     to trusting that header (no claims present) → forwards "tenant_id": identity.TenantID
     in the retrieve request body → retrieval-orchestrator.
  2. Any internal service (or attacker holding only SCOS_INTERNAL_SERVICE_TOKEN +
     SCOS_CONTROL_PLANE_TOKEN) → POST /v1/retrieve directly on retrieval-orchestrator
     with an arbitrary "tenant_id" in the JSON body → no header check, no claims check,
     no cross-check against any authenticated identity at all → DB query executes with
     attacker-chosen tenant_id.
  3. Any internal service holding SCOS_INTERNAL_SERVICE_TOKEN + control-plane token →
     POST/GET code-intelligence /v1/repos, /v1/query, /v1/impact, etc. with
     X-SCOS-Tenant-ID set to an arbitrary tenant → requireTenant() reads the header
     directly, never consults auth.CanonicalClaims → full cross-tenant repository/code
     graph read and write.
  4. Any internal service holding SCOS_INTERNAL_SERVICE_TOKEN (JWT claims not required —
     AuthenticatedHandler accepts the raw shared token) → GET /v1/traces/{id},
     /v1/metrics/query, /v1/logs on observability → zero tenant filtering exists in the
     handler code despite Tenant fields being defined on the stored structs → full
     cross-tenant read of all ingested metrics/traces/logs.
  5. agent-registry comparison workflow: same shared-token caller → any /v1/agents* or
     /v1/handoffs* endpoint with X-SCOS-Tenant-ID set → resolveIdentity() detects
     auth.Identity(ctx) != "" (set by AuthenticatedHandler for every successfully
     authenticated request, claims or not) and returns 401 unless the caller instead
     presents a real claims-bearing bearer token whose tenant_id/principal_id it then
     uses verbatim — the header fallback is dead code on the real HTTP path, reachable
     only from unit tests that call resolveIdentity() directly. Confirmed NOT exploitable
     via the network path.

Trust boundaries:
  - SCOS_INTERNAL_SERVICE_TOKEN: one shared secret held by every internal service;
    possession authenticates a caller as "some internal service" but carries no tenant
    or principal information by itself.
  - SCOS_CONTROL_PLANE_TOKEN: a second shared secret gating ControlPlaneHandler-wrapped
    services; also carries no tenant/principal information.
  - JWT-style claims bearer (3-part token, HMAC-signed with SCOS_SECURITY_ISSUER_SECRET):
    the only mechanism in this codebase that cryptographically binds tenant_id/principal_id
    to the caller.
  - X-SCOS-Tenant-ID / X-SCOS-Principal-ID headers and body tenant_id/principal_id fields:
    plain caller-supplied strings with no cryptographic binding to the authenticated
    identity unless a service explicitly cross-checks them against claims.

Attacker-controlled inputs:
  - X-SCOS-Tenant-ID / X-SCOS-Principal-ID request headers
  - JSON body tenant_id / principal_id fields (RetrieveRequest.TenantID, etc.)
  - Any value reachable by a compromised or malicious holder of the shared
    SCOS_INTERNAL_SERVICE_TOKEN (a large blast radius: every internal service process,
    and per the connection guide, execution-proxy and other agent-facing services that
    hold this token for outbound calls)

Invariants tested:
  - "Authorization must bind actor, tenant, and object at the point of effect" (skill's
    learned heuristic) — tested against every requireTenant/resolveIdentity-style function
    found in the eight target services.
  - Cross-tenant read/write should be impossible for a caller that only presents the
    shared internal token and self-declared tenant/principal values, without a
    claims-bearing token proving that binding.
  - RLS/set_config('scos.tenant_id', …) should be present wherever a service performs
    direct tenant-scoped SQL (per database/policies/ convention referenced in the task).

Findings:

  1. [CRITICAL] retrieval-orchestrator trusts an unauthenticated tenant_id/principal_id
     straight from the JSON request body — no header check, no claims check, no
     cross-check against any authenticated identity whatsoever, and no RLS/set_config
     usage in the file.
     - Location: services/retrieval-orchestrator/cmd/server/main.go:34 (RetrieveRequest.TenantID),
       190-197 (only checks TenantID/PrincipalID are non-empty), 320-950 (every DB query
       parameterized directly with req.TenantID).
     - Server wiring: main.go:167 wraps the mux only with auth.ControlPlaneHandler, which
       itself wraps auth.AuthenticatedHandler — neither ever inspects req.TenantID or
       compares it to auth.CanonicalClaims(r.Context()). Confirmed via
       `grep -n "CanonicalClaims\|claims\." services/retrieval-orchestrator/cmd/server/main.go`
       → zero matches in the whole file.
     - Attacker precondition: possession of SCOS_INTERNAL_SERVICE_TOKEN (the shared
       token used by every internal service) and SCOS_CONTROL_PLANE_TOKEN. Neither
       requires tenant-specific credentials.
     - Trace: POST /v1/retrieve → handleRetrieve() → decodes RetrieveRequest → validates
       non-empty → builds SQL with `WHERE mi.tenant_id = $1` etc., binding req.TenantID
       verbatim → returns matching memories/results for the requested tenant.
     - Violated invariant: authorization must bind actor+tenant+object at the effect
       boundary; here the "tenant" is a completely free-form attacker-chosen string.
     - Impact: any compromised internal service (or an attacker who obtains the shared
       token via any other in-scope finding, log leak, misconfigured deployment, etc.)
       can read (and, for ingest-style endpoints in the same file, write) any other
       tenant's retrieved memories by simply setting tenant_id in the request body.
       This is strictly worse than a header-trust bug because it requires no header at
       all — the value is taken from application-level JSON that every caller already
       controls.
     - Mitigation search: no compensating control found — no RLS session variable set,
       no claims cross-check, no allowlist of tenant IDs the caller is scoped to.
     - Confidence: Confirmed.

  2. [HIGH] code-intelligence's requireTenant() derives the authoritative tenant purely
     from the client-supplied X-SCOS-Tenant-ID header, with no call to
     auth.CanonicalClaims anywhere in the file, despite its own comment calling the
     header "the authenticated tenant header."
     - Location: services/code-intelligence/cmd/server/main.go:419-426 (requireTenant),
       used at lines 491, 554, 581, 730, 1089, 1202, 1314. `grep -n "CanonicalClaims"
       services/code-intelligence/cmd/server/main.go` → zero matches.
     - Server wiring: main.go:414 wraps with auth.ControlPlaneHandler (shared internal
       token + control-plane token), not a claims-only gate.
     - Trace: handleRepos() (line 490) explicitly trusts `tenant` from requireTenant()
       to scope both reads ("if repo.TenantID == tenant") and writes ("repo.TenantID =
       tenant" — the comment at line 518-520 claims this "prevent[s] a caller from
       registering a repository into another tenant's namespace," which is true only if
       the header itself can be trusted, which it cannot).
     - Impact: any caller holding the shared internal token + control-plane token can
       list, create, and presumably query/impact-analyze another tenant's code
       repositories, symbols, and indexed source by setting X-SCOS-Tenant-ID to the
       victim tenant. Full cross-tenant IDOR/BOLA across the code-intelligence surface.
     - Confidence: Confirmed (same mechanism as the already-known pattern in
       memory-intelligence/mcp-gateway/memory-gateway, independently re-verified here in
       a service not previously covered).

  3. [MEDIUM] observability has zero tenant enforcement on read paths despite defining a
     Tenant field on stored records, and is gated only by the shared internal token
     (auth.AuthenticatedHandler, no claims requirement).
     - Location: services/observability/cmd/server/main.go:41,60,75 (Tenant field on
       MetricPoint/TraceSpan/LogRecord-style structs), handleMetricsQuery (541-567,
       filters only on name/service/since), handleTraces (571-576, returns the entire
       in-memory slice), handleTraceByID (635-653, filters only on TraceID, no tenant
       check), server wiring at line 354.
     - Trace: any caller with the shared token can GET /v1/traces, /v1/metrics/query, or
       /v1/traces/{id} and receive spans/metrics/logs for every tenant, including
       whatever operational content (which may include prompts, tool arguments, or
       other sensitive context per the fields captured) was ingested for other tenants.
     - Impact: cross-tenant observability data exposure. Lower severity than findings 1-2
       because the payload is operational telemetry rather than primary tenant data, but
       still a tenant-isolation violation reachable by the same broad set of internal
       callers.
     - Confidence: Confirmed for lack of tenant filtering; High-confidence (not
       Confirmed) that this rises to the same severity class as 1-2, since the
       sensitivity of ingested telemetry content wasn't independently verified against
       every producer service.

Suspicions requiring validation:
  - services/backup-manager and services/recovery-orchestrator: no tenant concept exists
    in either service (`grep -n "Tenant\|tenant"` returns zero hits in both main.go
    files outside tests). This may be intentional — both operate at whole-database/
    whole-instance granularity as infra-level admin tools, not tenant-scoped resources —
    but it does mean any caller with the shared SCOS_INTERNAL_SERVICE_TOKEN can trigger
    backup/restore of the *entire* multi-tenant dataset (backup-manager's handleBackups/
    handleRestore, main.go:298-459) with no additional privilege check beyond
    auth.AuthenticatedHandler. Flagging as a suspicion rather than a finding because it
    was out of the explicit "identity-spoofing" mechanism this pass targeted, and I did
    not verify whether a higher-privilege token/role is enforced elsewhere (e.g. at a
    network/deployment layer) before a caller can reach these services at all.
    Confidence: Plausible — recommend a dedicated pass (hostile-configuration-auditor /
    hostile-blind-spot-auditor territory) on whether backup-manager and
    recovery-orchestrator should require a distinct, higher-privilege admin credential
    rather than the same shared token every ordinary internal service holds.
  - context-optimisation's handleContextRestore (main.go:284-315) reads from a global,
    non-tenant-partitioned fileSourceStore keyed only by ObjectID+Version, gated by a
    64-hex-char ContentHash equality check (validSourceRef, 319-328). Exploiting this for
    cross-tenant data access would require the attacker to already know a valid
    ObjectID/Version/ContentHash triple for another tenant's capsule — practically
    equivalent to already possessing the plaintext (SHA-256 preimage resistance).
    Confidence: Hardening opportunity, not a real cross-tenant read path under current
    threat model; noted because the store itself has no tenant field and would become
    exploitable if ObjectIDs ever became guessable/sequential or content hashes weakened.
  - mcp-gateway's resolveIdentity() (main.go:794-807) still exhibits the previously
    documented header-trust fallback (falls back to X-SCOS-Tenant-ID/X-SCOS-Principal-ID
    whenever no claims are present) and, combined with copyIdentityHeaders() (780-792)
    and the literal "tenant_id": identity.TenantID forwarding at line 606, forms the
    first hop of the exploit chain into retrieval-orchestrator (finding 1). Restated only
    to document the chain, per instructions not to re-litigate the already-known
    mcp-gateway finding itself.
  - graphify-provider: confirmed NOT vulnerable to this pattern — s.tenantID/
    s.principalID (graphify.go:96-97, used in postJSON) are populated once at startup
    from GRAPHIFY_TENANT_ID (main.go:77), not from any per-request attacker input, so
    outbound calls to peer services carry a statically configured identity rather than a
    spoofable one.
  - agent-registry: confirmed NOT vulnerable via the real HTTP path. resolveIdentity()
    (main.go:104-126) is claims-first and explicitly 401s a shared-token caller that has
    no claims, even if X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers are present
    (main.go:111-113). The header-only fallback is reachable only by unit tests that
    invoke resolveIdentity() directly, bypassing auth.AuthenticatedHandler (confirmed via
    main.go:192, which wraps the mux with auth.AuthenticatedHandler for every route).
    This is the correct/fixed shape of this pattern and was used as the comparison
    baseline for findings 1-3.

Tests/commands executed:
  - grep -rn "X-SCOS-Tenant-ID\|X-SCOS-Principal-ID\|resolveIdentity\|ResolveIdentity"
    across all eight in-scope services (see Workflows traced)
  - grep -n "CanonicalClaims\|claims\." services/retrieval-orchestrator/cmd/server/main.go
    → 0 matches
  - grep -n "CanonicalClaims" services/code-intelligence/cmd/server/main.go → 0 matches
  - grep -n "Tenant|tenant" services/backup-manager/cmd/server/main.go and
    services/recovery-orchestrator/cmd/server/main.go → 0 matches (non-test)
  - grep -n "\.Tenant\b" services/observability/cmd/server/main.go → 0 matches (field
    defined but never read/filtered on)
  - grep -n "set_config|scos.tenant_id|BeginTx|setTenantContext"
    services/retrieval-orchestrator/cmd/server/main.go → 0 matches (no RLS session
    binding in this service)
  - Read of packages/auth/middleware.go in full to establish the shared-token vs.
    claims-token trust model used as the baseline for every other check.
  - Read of services/agent-registry/cmd/server/main.go:80-150 and main_test.go:138-151
    to confirm the header-fallback path is test-only, not network-reachable.
  - Read of services/graphify-provider/cmd/server/{graphify.go,main.go} to confirm
    tenantID/principalID are startup-configured, not request-derived.
  - No live services were started; all findings are static-analysis-derived from
    source. No `go test` / `go build` was run in this pass (see Coverage gaps).

Coverage gaps:
  - Did not build or run any of the eight services' test suites (main_test.go files
    exist for agent-registry and code-intelligence; not executed).
  - Did not trace whether database/policies/ RLS policies exist for the tables
    retrieval-orchestrator queries (memories, namespaces, etc.) that might independently
    enforce tenant_id at the Postgres layer even though the Go code never calls
    set_config. If such a policy exists and is enforced via a role/session mechanism
    unrelated to set_config, it could partially mitigate finding 1 — not verified here.
  - Did not fully audit context-optimisation's remaining handlers (handleOutputOptimise
    body beyond line 428, and any endpoints after line 430) or recovery-orchestrator's
    and backup-manager's complete handler bodies beyond the function-name inventory —
    only spot-checked for tenant-related identifiers.
  - Did not verify network-level reachability constraints (e.g., whether
    retrieval-orchestrator, code-intelligence, or observability are actually reachable
    from outside a private network/mesh, which would change exploitability of findings
    1-3 without changing the code-level defect).
  - Did not check whether execution-proxy or tool-governance (named in background
    context as reachable by agents) can reach retrieval-orchestrator or code-intelligence
    directly, which would shorten the attacker path further.
  - Did not examine memory-gateway, memory-intelligence, or mcp-gateway beyond the
    minimum needed to trace the exploit chain into retrieval-orchestrator, per task
    instructions treating them as background/already-covered.

Areas explicitly not reviewed:
  - autonomous-control-plane, compatibility-gateway, execution-proxy, extension-manager,
    hermes-provider, okf-synchroniser, security-gate, tool-governance,
    transparency-projection — out of the explicit scope hint for this pass.
  - Frontend/UI code, deployment manifests, and CI configuration.
  - database/policies/*.sql RLS definitions themselves (only referenced as background).

Recommended follow-up:
  - Immediate: add an auth.CanonicalClaims-based cross-check to
    retrieval-orchestrator's handleRetrieve (finding 1) and code-intelligence's
    requireTenant (finding 2), mirroring agent-registry's resolveIdentity() pattern:
    require a claims-bearing token for any tenant-scoped operation, and reject (or at
    minimum log-and-flag) a shared-token-only caller that supplies tenant identity via
    header or body.
  - Add tenant filtering to observability's read handlers (finding 3), or explicitly
    document/enforce that observability is a cross-tenant-by-design operational store
    and restrict which callers may reach it.
  - Escalate findings 1 and 2 to the master auditor as Critical/High cross-tenant
    exploitable chains per the skill's escalation conditions, and note the exploit chain
    through mcp-gateway → retrieval-orchestrator connects an already-known finding to a
    new, more severe one in a different service.
  - Follow up on the backup-manager/recovery-orchestrator suspicion with a
    configuration-focused pass to determine whether whole-database backup/restore should
    require a distinct credential from ordinary inter-service calls.
  - Run the existing agent-registry and code-intelligence test suites and add a
    regression test for code-intelligence equivalent to
    TestResolveIdentityRequiresTenantAndPrincipal / TestResolveIdentityFromHeaders once
    the fix lands, to prevent silent regression back to header trust.
```
