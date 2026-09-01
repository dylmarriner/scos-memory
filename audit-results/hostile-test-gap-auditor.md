# Specialist audit artifact

```text
Skill: hostile-test-gap-auditor
Agent: hostile-test-gap-auditor-workflow-agent
Scope: tests/ (failure-injection, stress, test_audit_governance_runtime.py) and *_test.go under services/ and packages/,
  cross-referenced against packages/auth/middleware.go, services/execution-proxy, services/tool-governance,
  services/mcp-gateway, and database/policies (RLS), plus .github/workflows/ci.yml to determine what actually runs in CI.

Files/components inspected:
  - packages/auth/middleware.go, packages/auth/middleware_test.go
  - services/execution-proxy/cmd/server/main.go, main_test.go
  - services/tool-governance/cmd/server/main.go, main_test.go
  - services/mcp-gateway/cmd/server/main.go, main_test.go
  - database/policies/000001..000009_*.sql
  - database/integration/rls_test.go (build-tagged `integration`)
  - .github/workflows/ci.yml
  - docs/agent-connection-guide.md
  - deploy/compose/docker-compose.yml, deploy/native/env/scos-fabric.env.example
  - tests/test_audit_governance_runtime.py, tests/failure-injection/*, tests/stress/*

Entry points inspected:
  - auth.AuthenticatedHandler / auth.ControlPlaneHandler / auth.validateClaims (packages/auth/middleware.go:108-226)
  - POST /v1/execute, validateExecutionRequest, planCommand, classifyRisk (services/execution-proxy/cmd/server/main.go:534-560, 897-939)
  - POST /mcp handleMCP -> handleToolsCall -> resolveIdentity -> dispatchTool (services/mcp-gateway/cmd/server/main.go:157-236, 794-805)
  - tool-governance resolveIdentity / handleToolRegister (services/tool-governance/cmd/server/main.go, main_test.go:79-98)

Important callers/callees:
  - mcp-gateway dispatchTool forwards X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers downstream to hermes-provider, memory-gateway, etc.
    (main.go:227-236, copyIdentityHeaders:780-793) — downstream services trust these headers as the tenant/principal identity for RLS
    scoping (set_config('scos.tenant_id', ...)).
  - execution-proxy validateExecutionRequest is the single choke point that must reject destructive commands without
    confirmation and identity spoofing before governance/execution.
  - database/policies/*.sql RLS policies are the last line of defense if application-level tenant checks are bypassed.

Workflows traced:
  1. External client -> mcp-gateway /mcp (Bearer <SCOS token>, X-SCOS-Tenant-ID, X-SCOS-Principal-ID) -> resolveIdentity
     -> dispatchTool -> downstream service -> Postgres RLS.
  2. Agent -> execution-proxy /v1/execute (tenant_id/principal_id in body + matching headers) -> validateExecutionRequest
     -> planCommand/classifyRisk -> tool-governance AuthorizeAndPrepare -> handleExecute executes shell command.
  3. CI: .github/workflows/ci.yml `go test ./...` (no `-tags=integration`, no DATABASE_URL) plus Python unittest discovery.

Trust boundaries:
  - External client -> mcp-gateway (only boundary reachable by an untrusted actor per docs/agent-connection-guide.md).
  - mcp-gateway -> downstream internal services (shared bearer token boundary).
  - execution-proxy -> host shell (command execution boundary; execution-proxy runs arbitrary agent-issued commands).
  - Application layer -> Postgres RLS (defense-in-depth boundary keyed on scos.tenant_id session GUC).

Attacker-controlled inputs:
  - Authorization header value (shared SCOS_INTERNAL_SERVICE_TOKEN or claims JWT) presented by any holder of a valid credential.
  - X-SCOS-Tenant-ID / X-SCOS-Principal-ID request headers (raw, unsigned, client-supplied strings).
  - ExecuteRequest.TenantID/PrincipalID/Command/Confirmed/Mode JSON body fields on /v1/execute.
  - JWT claims payload (tenant_id, principal_id, exp) for the claims-token path in validateClaims.

Invariants tested:
  - AuthenticatedHandler rejects missing/incorrect static bearer token (middleware_test.go:21-48).
  - AuthenticatedHandler exposes canonical claims from a validly-signed JWT with no exp (middleware_test.go:63-79).
  - ControlPlaneHandler rejects a caller presenting only the service bearer token without the separate control-plane token
    (middleware_test.go:81-108).
  - execution-proxy classifies "rm -rf" style commands as destructive and defaults them to forensic mode /
    passthrough-without-rewrite (main_test.go:51-60, 98-132).
  - execution-proxy fails closed on unprepared/unauthorized governance operations (main_test.go:473-496).
  - database/integration/rls_test.go proves, when actually executed against a real Postgres instance, that cross-tenant
    reads/writes across ~24 governed tables are blocked by RLS (rls_test.go:39-181).

Findings:
  - id: TG-1
    title: mcp-gateway trusts client-supplied X-SCOS-Tenant-ID/Principal-ID headers with no test proving they are bound
           to the authenticated caller; deployment model hands the same shared token to all external clients
    severity: Critical (design/architecture level, confirmed by code+docs; reported here as the corresponding *test gap*)
    location: services/mcp-gateway/cmd/server/main.go:797-805 (resolveIdentity); docs/agent-connection-guide.md:144-146
    evidence: |
      resolveIdentity() first checks auth.CanonicalClaims(ctx) (populated only when the bearer token is a 3-part JWT
      validated against SCOS_SECURITY_ISSUER_SECRET). If claims are absent -- i.e. the caller authenticated with the
      plain SCOS_INTERNAL_SERVICE_TOKEN, which docs/agent-connection-guide.md:144-146 explicitly instructs external
      clients to send alongside raw X-SCOS-Tenant-ID / X-SCOS-Principal-ID headers -- resolveIdentity falls back to
      trusting those headers verbatim with zero cryptographic binding to the caller's actual tenant/principal.
      Both SCOS_INTERNAL_SERVICE_TOKEN and SCOS_SECURITY_ISSUER_SECRET are required by deploy/compose/docker-compose.yml,
      and AuthenticatedHandler (packages/auth/middleware.go:149-158) accepts either credential form on the same endpoint,
      so a deployment is not forced onto the JWT-only path.
      No test anywhere in the repository (main_test.go for mcp-gateway, execution-proxy, tool-governance, or the
      auth package) exercises "valid static-token bearer auth + forged X-SCOS-Tenant-ID header for a different tenant"
      and asserts rejection. TestHandleMCPToolsCallRequiresIdentity only proves an *absent* header is rejected
      (main_test.go:91-109); TestResolveIdentityFallsBackToHeaders (main_test.go:146-157) exercises exactly the
      spoofable fallback path and asserts it *succeeds*, without asserting anything ties the header value to a caller
      identity.
    impact: If a shared static token is issued to more than one tenant's external agent (the natural reading of
      "external Claude Code/Desktop clients" all configuring SCOS_HERMES_BEARER_TOKEN=<internal service token>"),
      any such client can set X-SCOS-Tenant-ID to another tenant's UUID and reach that tenant's data/tools through
      every downstream service that trusts the forwarded header, before RLS is ever consulted.
    confidence: High-confidence (code path and docs both confirmed directly; whether tokens are provisioned per-tenant
      in the live deployment was not independently verifiable from this repo, hence not "Confirmed")

  - id: TG-2
    title: No test proves execution-proxy rejects a destructive command lacking explicit confirmation
    severity: High
    location: services/execution-proxy/cmd/server/main.go:554-557 (validateExecutionRequest); main_test.go (whole file)
    evidence: |
      validateExecutionRequest() requires req.Confirmed == true when classifyRisk(command) == "destructive"
      ("explicit confirmation is required for destructive commands"). Every existing destructive-command test
      (TestClassifyRiskDestructive, TestPlanCommandDestructivePassthrough, TestEffectiveModeDefaultsDestructiveToForensic,
      main_test.go:51-60, 98-111, 126-132) calls classifyRisk/planCommand directly and never goes through
      validateExecutionRequest or the HTTP handleExecute path with Confirmed unset/false. No test asserts the
      400/403 outcome for an unconfirmed destructive command, so a future refactor could silently drop this check
      with no test to catch it.
    impact: A regression that removes or weakens the confirmation gate for destructive commands (e.g. "rm -rf") would
      pass the full test suite undetected.
    confidence: Confirmed (absence verified by reading every test function name and body in main_test.go)

  - id: TG-3
    title: No test proves execution-proxy rejects a request whose body tenant_id/principal_id mismatches the
           authenticated X-SCOS-Tenant-ID/Principal-ID headers
    severity: High
    location: services/execution-proxy/cmd/server/main.go:544-550 (validateExecutionRequest)
    evidence: |
      Lines 548-550 reject when `tenant != req.TenantID || principal != req.PrincipalID`. grep across
      main_test.go shows every test sets header tenant/principal equal to the body values ("tenant"/"principal" in
      every case); none constructs a mismatch to prove the guard fires. TestModeRejectedWhenInvalid is the only
      direct caller of validateExecutionRequest and it uses matching identities.
    impact: A cross-tenant impersonation attempt via mismatched body vs. header identity (e.g. after the mcp-gateway
      forwards attacker-controlled headers per TG-1) would not be caught by CI if this specific check regressed.
    confidence: Confirmed (absence verified directly in main_test.go)

  - id: TG-4
    title: No test anywhere in the repository exercises an expired bearer JWT
    severity: High
    location: packages/auth/middleware.go:213-215 (validateClaims exp check); packages/auth/middleware_test.go (whole file)
    evidence: |
      validateClaims rejects when `raw.Exp > 0 && !time.Now().UTC().Before(time.Unix(raw.Exp, 0))`. The only claims
      test, TestAuthenticatedHandlerUsesSignedCanonicalClaims (middleware_test.go:63-79), builds a payload with no
      "exp" field at all (`{"tenant_id":"tenant-a","principal_id":"principal-a"}`), so raw.Exp == 0 and the expiry
      branch is never taken. A repo-wide grep for "expired" / an exp-based test across every *_test.go file returned
      no other results.
    impact: A regression in the expiry comparison (e.g. an inverted condition, or expiry disabled) would ship
      undetected; expired-credential replay is a standard attack against any bearer-token scheme.
    confidence: Confirmed (absence verified by grep across the full repository)

  - id: TG-5
    title: No test exercises a malformed/tampered claims JWT (bad signature, non-JWT header segment, non-base64
           payload, or missing tenant_id/principal_id claims) reaching AuthenticatedHandler
    severity: Medium
    location: packages/auth/middleware.go:173-226 (validateClaims); packages/auth/middleware_test.go
    evidence: |
      validateClaims has five distinct failure branches: wrong header segment/part count (line 175), invalid
      base64 signature (178-180), signature/HMAC mismatch (182-185), invalid base64 payload (187-189), and missing
      tenant_id/principal_id claims (210-212). Every existing test only produces well-formed tokens via the
      testJWT() helper with a correct secret; none constructs a token with a wrong secret, a corrupted signature,
      or an empty tenant_id/principal_id and asserts 401. TestAuthenticatedHandlerRequiresBearerToken only tests the
      static-token path ("no"/"secret"), not the three-segment claims-token path.
    impact: A parsing or signature-verification bypass in validateClaims would not be caught by the current suite.
      Lower severity than TG-4 because the code paths are short and independently look correct on inspection, but
      still unverified by any regression test.
    confidence: Confirmed (absence verified directly in middleware_test.go)

  - id: TG-6
    title: The only real RLS/tenant-isolation test suite in the repository is excluded from CI by default
    severity: High
    location: database/integration/rls_test.go:1 (`//go:build integration`); .github/workflows/ci.yml:33-34
    evidence: |
      database/integration/rls_test.go is the single test in the repo that actually connects to Postgres, applies
      the real RLS policies from database/policies/, and asserts cross-tenant SELECT/INSERT/UPDATE are blocked
      (rls_test.go:39-181, e.g. "cross tenant writes are rejected" at line 152). It is gated behind the `integration`
      build tag and requires a live `DATABASE_URL` (line 40-42). .github/workflows/ci.yml's Go job runs plain
      `go test ./...` (line 34) with no `-tags=integration` and no DATABASE_URL configured anywhere in the workflow,
      and no other CI job (python, security, docker) runs it either.
    impact: A change to database/policies/*.sql (or a migration ordering bug) that broke RLS tenant isolation would
      not fail CI. This is the exact scenario the RLS test suite exists to catch, and it silently does not run.
    confidence: Confirmed (build tag and CI workflow both read directly)

Suspicions requiring validation:
  - Whether SCOS_INTERNAL_SERVICE_TOKEN is provisioned per-tenant or as one global shared secret across all external
    agents in the actual production deployment (TG-1's severity hinges on this; not determinable from this repo alone
    -- deploy/native/env/scos-fabric.env.example and docker-compose.yml both show a single token value per environment,
    suggesting one shared secret, but live provisioning practice was not inspected).
  - Whether tool-governance's AuthorizeAndPrepare (called downstream of execution-proxy) independently re-validates
    tenant/principal against its own stored operation record in a way that would catch a TG-3-style spoof even if
    validateExecutionRequest regressed -- not fully traced in this pass (see coverage gaps).

Tests/commands executed:
  - Read/inspected only (no test execution performed in this session); findings are based on static reading of test
    files and reachable source, and on `grep`/`find` inventory commands listed under Files/components inspected.
  - Commands run: `find tests -type f`, `find services packages -name "*_test.go"`, `grep -n "func Test" <files>`,
    `grep -rn "current_setting|scos.tenant_id|set_config" ...`, `grep -n ... .github/workflows/ci.yml`.

Coverage gaps:
  - Did not read every *_test.go in the inventory (56 Go test files total) line-by-line; focused on auth,
    execution-proxy, tool-governance, mcp-gateway, and RLS per the task's scope hint. Other services (agent-registry,
    backup-manager, recovery-orchestrator, memory-gateway, etc.) were not individually audited for negative/auth/tenant
    test gaps.
  - Did not trace tool-governance's AuthorizeAndPrepare / Reconcile implementation in depth to confirm whether it
    provides an independent tenant check that would mitigate TG-1/TG-3 if execution-proxy's own check were bypassed.
  - Did not run `go test ./... -tags=integration` against a live Postgres instance to confirm rls_test.go currently
    passes (only confirmed it is excluded from the checked-in CI config).
  - Did not inspect workers/ (consolidation, embeddings, index-maintenance, policy-evaluation) Python test suites for
    tenant/auth negative-test gaps.
  - Did not review tests/stress/runner.py or tests/failure-injection/runner.py content in depth (only confirmed their
    existence and that they are Python failure/stress harnesses, not auth/tenant negative tests).

Areas explicitly not reviewed:
  - packages/database (pool_test.go, tls_test.go) beyond confirming they exist.
  - services/backup-manager, recovery-orchestrator, memory-gateway, memory-intelligence, hermes-provider,
    compatibility-gateway, agent-registry, okf-synchroniser, observability, security-gate, autonomous-control-plane,
    context-optimisation, retrieval-orchestrator, transparency-projection, code-intelligence, graphify-provider,
    extension-manager test suites.
  - packages/agent-adapter-sdk, benchmark-harness, capability-registry, context-contracts, execution-adapters,
    extension-contracts, hermes-memory-provider, hook-sdk, httpmid, language-adapters, routing-contracts,
    service-state, skill-sdk, telemetry, tlsserve test suites.

Recommended follow-up:
  - Add an execution-proxy test asserting a destructive command with Confirmed=false (or omitted) is rejected by
    validateExecutionRequest / handleExecute with 400/403 (closes TG-2).
  - Add an execution-proxy test asserting a request whose body tenant_id/principal_id differs from the
    X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers is rejected (closes TG-3).
  - Add an auth-package test constructing a JWT with `"exp"` set in the past and asserting AuthenticatedHandler
    returns 401 (closes TG-4).
  - Add auth-package tests for a wrong-secret signature, a corrupted base64 segment, and a payload missing
    tenant_id/principal_id, each asserting 401 (closes TG-5).
  - Wire database/integration/rls_test.go into CI (a dedicated job with a Postgres service container, DATABASE_URL,
    and `go test -tags=integration ./database/...`) so RLS regressions are caught automatically (closes TG-6).
  - Escalate TG-1 to the authorization/authentication specialist auditors and to the master auditor: determine
    definitively whether SCOS_INTERNAL_SERVICE_TOKEN is single-shared-secret-for-all-tenants in production, and if so,
    require mcp-gateway to derive tenant/principal only from cryptographically verified claims (reject requests that
    fall back to unauthenticated X-SCOS-* headers when no JWT claims are present), with a regression test proving a
    forged tenant header is rejected even with a valid static token.
```
