# Specialist audit artifact

```text
Skill: hostile-authentication-identity-auditor
Agent: hostile-authentication-identity-auditor-workflow-agent

Scope:
- packages/auth (token issuance/verification/revocation lifecycle shared by every service)
- services/security-gate (token issue/validate/revoke endpoints, permission checks, secret resolution)
- services/mcp-gateway (externally-reachable identity propagation into the internal mesh)
- deploy/compose/docker-compose.yml and docs/agent-connection-guide.md (how the two shared
  credentials are distributed to ~20 services and to external agents)

Files/components inspected:
- packages/auth/middleware.go (full read)
- packages/auth/middleware_test.go (full read)
- services/security-gate/cmd/server/main.go (full read)
- services/security-gate/cmd/server/store.go (full read)
- services/mcp-gateway/cmd/server/main.go (handleMCP, handleToolsCall dispatch table,
  forwardPOST/forwardGET/forwardWithBody, copyIdentityHeaders, resolveIdentity)
- deploy/compose/docker-compose.yml (lines 1-40: x-service-auth-environment anchor; line 834)
- docs/agent-connection-guide.md (lines 1-100)
- services/memory-gateway/cmd/server/outbox.go (spot check of async tenant propagation)
- .env.example, deploy/native/env/scos-fabric.env.example (credential distribution)

Entry points inspected:
- packages/auth.AuthenticatedHandler — wraps every service's HTTP mux except /health/live,
  /health/ready (used by security-gate, mcp-gateway, and per the shared compose env, all
  ~20 services)
- packages/auth.ControlPlaneHandler — additional gate for control-plane-only services
  (wraps AuthenticatedHandler)
- POST /v1/auth/issue, /v1/auth/validate, /v1/auth/revoke (security-gate)
- POST /v1/permissions/check, /v1/permissions/classify (security-gate)
- POST /mcp (mcp-gateway, JSON-RPC 2.0 tools/call), specifically scos_auth_issue,
  scos_auth_validate, scos_auth_revoke forwarding

Important callers/callees:
- packages/auth.FromEnvironment() -> os.Getenv("SCOS_INTERNAL_SERVICE_TOKEN"),
  os.Getenv("SCOS_SECURITY_ISSUER_SECRET"), os.Getenv("SCOS_CONTROL_PLANE_TOKEN")
- packages/auth.AuthenticatedHandler -> packages/auth.validateClaims (stateless HMAC check,
  no DB lookup, no revocation list)
- services/security-gate main.go:233 -> auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())
  wraps the ENTIRE security-gate mux, including /v1/auth/issue itself
- services/security-gate handleAuthIssue/handleAuthValidate/handleAuthRevoke ->
  store.go insertToken/lookupToken/revokeToken -> security_tokens Postgres table
  (a DB-backed, revocable token type, distinct from the AuthenticatedHandler credential)
- services/mcp-gateway handleToolsCall -> forwardPOST -> copyIdentityHeaders (forwards only
  Authorization, X-SCOS-Tenant-ID, X-SCOS-Principal-ID, Idempotency-Key; does NOT forward
  X-SCOS-Issuer-Secret)

Workflows traced:
1. Static shared-secret path: client presents `Authorization: Bearer <SCOS_INTERNAL_SERVICE_TOKEN>`
   -> AuthenticatedHandler does subtle.ConstantTimeCompare against config.Token -> ctx identity
   set to config.Identity (a fixed string, e.g. "internal-service") -> no tenant/principal
   granularity, no expiry, no per-caller identifier.
2. Claims-token path: client presents a 3-part `header.payload.sig` bearer where
   config.IssuerSecret is configured -> validateClaims re-derives HMAC-SHA256 over
   header+payload using SCOS_SECURITY_ISSUER_SECRET and compares to the supplied signature
   -> on match, decodes tenant_id/principal_id/exp from the payload directly, no DB lookup,
   no jti/blocklist, exp enforced only `if raw.Exp > 0`.
3. Token minting: security-gate handleAuthIssue mints a DIFFERENT token type (public-id +
   HMAC over the id, using the same issuerSecret field but a different format/verb),
   persisted in security_tokens with expires_at and revoked_at columns, exposed only through
   /v1/auth/issue|validate|revoke and consumed only by security-gate's own
   handlePermissionCheck/handleClassifyData via authenticatedToken(). This system supports
   real revocation (store.go:158 revokeToken sets revoked_at, checked by lookupToken).
   It is layered ON TOP OF workflow 1/2 (a caller must already have passed
   AuthenticatedHandler to reach handleAuthIssue at all) and does not replace or protect
   the shared credentials used in workflow 1/2.
4. External exposure: mcp-gateway (port 8098, only service documented as directly reachable
   by external MCP clients — docs/agent-connection-guide.md) is gated purely by workflow 1/2
   (services/mcp-gateway/cmd/server/main.go:115). docs/agent-connection-guide.md:52 instructs
   operators to place "internal service token or claims JWT" in
   ~/.hermes/scos.env (mode 600) on each machine running Claude Code, Codex, Kilo, OpenCode,
   OpenClaw, Qwen, or hermes-agent, all sharing one file/one credential.

Trust boundaries:
- External MCP client (Claude Code/Desktop, Codex, Kilo, OpenCode, OpenClaw, Qwen,
  hermes-agent processes on arbitrary developer machines) -> mcp-gateway (public boundary)
- mcp-gateway -> every other internal service (forwards caller's own Authorization header
  verbatim; does not mint a narrower-scoped downstream credential)
- Any of the ~20 services (all hold SCOS_SECURITY_ISSUER_SECRET per the shared compose
  environment anchor) -> every other service's tenant/principal-scoped Postgres RLS boundary,
  because validateClaims trusts the tenant_id/principal_id embedded in a claims token signed
  with that single shared secret, with no issuer/audience restriction tying a token to the
  service that was supposed to mint it.

Attacker-controlled inputs:
- Authorization header bearer value (static token or 3-part claims token) on every request
  to every service
- JSON body of /v1/auth/issue (principal, tenant, agent_id, session_id, expires_in_seconds) —
  gated by X-SCOS-Issuer-Secret header, confirmed NOT forwarded by mcp-gateway's
  copyIdentityHeaders, so this specific mint path is not reachable through the external
  MCP surface (verified, not just assumed)
- exp claim (or its absence) inside a claims token payload

Invariants tested:
- "A revoked credential can no longer authenticate." True only for the DB-backed
  security_tokens type (security-gate's own /v1/auth/* system). FALSE for the two credential
  types that actually gate every service's HTTP surface (SCOS_INTERNAL_SERVICE_TOKEN static
  bearer, and SCOS_SECURITY_ISSUER_SECRET-signed claims tokens) — see Findings AUTH-1.
- "A claims token cannot outlive its issuer's intended lifetime." FALSE when `exp` is 0/absent
  — see Findings AUTH-3.
- "Only security-gate can mint claims tokens accepted fleet-wide." FALSE — see Findings AUTH-2;
  any of the ~20 services holding SCOS_SECURITY_ISSUER_SECRET can compute a valid signature
  locally without calling security-gate.
- "External, less-trusted MCP clients get a narrower-scoped credential than internal
  service-to-service calls." Not enforced in code; documentation explicitly allows the
  opposite — see Findings AUTH-4.

Findings:

[AUTH-1] CRITICAL — No revocation path for the two credentials that actually gate every
service (SCOS_INTERNAL_SERVICE_TOKEN, SCOS_SECURITY_ISSUER_SECRET).
- Location: packages/auth/middleware.go:132-171 (AuthenticatedHandler),
  packages/auth/middleware.go:173-226 (validateClaims), corroborated by
  deploy/compose/docker-compose.yml:1-14 (x-service-auth-environment anchor broadcasting both
  vars, unconditionally required, to every one of the ~20 services in docker-compose.yml).
- Evidence: AuthenticatedHandler's static-token branch (line 155) is a bare
  subtle.ConstantTimeCompare against an env-sourced string with no id, no persisted state,
  and no lookup of any kind. The claims branch (validateClaims) re-derives an HMAC signature
  from the payload and the shared secret with no database/cache lookup, no jti, no blocklist.
  The only revocation code that exists anywhere in the auth surface
  (services/security-gate/cmd/server/store.go:158 revokeToken, wired to
  POST /v1/auth/revoke in main.go:412-439) operates on a structurally different, DB-backed
  token type (security_tokens table) that only security-gate's own handlers consult
  (authenticatedToken() at main.go:360-375) — it has no relationship to
  SCOS_INTERNAL_SERVICE_TOKEN or SCOS_SECURITY_ISSUER_SECRET and cannot invalidate either.
- Reachable entry point: every non-health route on every service in the fleet.
- Attacker preconditions: possession of either shared secret (e.g. via a leaked env file,
  a compromised container, a leaked ~/.hermes/scos.env on any developer machine per AUTH-4,
  or a compromised service such as execution-proxy which the background context notes
  executes shell commands on behalf of agents).
- Impact: once either secret is known to an attacker, the only remediation is rotating the
  environment variable and redeploying every one of the ~20 services (confirmed necessary
  because the value is read fresh from os.Getenv on each process start via FromEnvironment,
  with no runtime revalidation, and there is no server-side state to invalidate). This
  directly confirms the scope hint's question: there is no way to invalidate
  SCOS_INTERNAL_SERVICE_TOKEN short of redeploy.
- Confidence: Confirmed (read the full validation and only-existing-revocation code paths).

[AUTH-2] HIGH — SCOS_SECURITY_ISSUER_SECRET is a single symmetric secret held by every
service in the fleet, letting any one of them forge claims tokens for any tenant/principal
without ever calling security-gate.
- Location: packages/auth/middleware.go:173-226 (validateClaims — no issuer, audience, or
  origin-service restriction beyond the raw HMAC signature); deploy/compose/docker-compose.yml
  lines 1-14 and 834 (the secret is placed in the shared x-service-auth-environment anchor,
  required — `:?...must be set` — for every service, not scoped to security-gate alone,
  with an explicit comment at lines 5-13 confirming this was a deliberate widening: "It was
  previously wired only into security-gate, which issues the tokens but never needs to verify
  one of its own.").
- Evidence: grep confirms only packages/auth/middleware.go, security-gate, and
  transparency-projection reference the env var name in Go source, but the compose anchor
  proves the *value* is injected into every service's process environment regardless of
  whether that service's source references it by name — any service can call
  packages/auth.FromEnvironment() (or replicate the seven-line HMAC computation in
  middleware.go:182-186) to mint a token accepted by every other service.
- Reachable entry point: any code path inside any of the ~20 services capable of making an
  outbound HTTP call or executing arbitrary logic (most directly execution-proxy, which the
  background context establishes executes shell commands on behalf of agents).
- Attacker preconditions: code execution or secret-read capability inside any one service in
  the mesh (not necessarily security-gate).
- Impact: full cross-tenant impersonation — a forged claims token can set tenant_id/
  principal_id to any value (isUUID validation in security-gate's handleAuthIssue at
  main.go:280-286 is bypassed entirely, since the forgery never touches that endpoint), and
  every downstream service trusts those claims to set Postgres RLS context
  (services/security-gate/cmd/server/store.go:90-93 setPrincipalContext, and the
  database/policies/ RLS design noted in the audit background) — i.e. this single shared
  secret is the de facto root of tenant isolation for the whole fabric.
- Confidence: Confirmed for the "no audience/origin restriction" mechanism; High-confidence
  for "any service can exploit this" specifically because it requires an additional
  precondition (compromise of some service), which is plausible but not independently proven
  in this pass.

[AUTH-3] HIGH — A claims token with no `exp` claim (or exp=0) is accepted with no expiry
enforced, ever.
- Location: packages/auth/middleware.go:213 —
  `if raw.Exp > 0 && !time.Now().UTC().Before(time.Unix(raw.Exp, 0)) { return ... "expired" }`.
- Evidence: the expiry check is gated behind `raw.Exp > 0`; when Exp is the zero value
  (claim absent from payload, or explicitly 0), the branch is skipped entirely and
  validateClaims proceeds to return a valid Claims{} (the ExpiresAt field computed at
  line 220 — `now + 5*time.Minute` — is informational only; it is never itself checked
  against on a *subsequent* request, since every request re-validates the same self-contained
  token from scratch with no session/state). packages/auth/middleware_test.go:63-79
  (TestAuthenticatedHandlerUsesSignedCanonicalClaims) constructs its test JWT with a payload
  containing no exp field at all and asserts success, confirming this is the intended,
  tested behavior rather than an oversight caught by existing tests.
- Reachable entry point: same as AUTH-1/AUTH-2 — any bearer-token-gated route on any service.
- Attacker preconditions: same as AUTH-2 (ability to sign a claims token with the shared
  secret) OR simply an over-privileged legitimate issuer omitting exp (e.g. a future caller
  of security-gate's minting path that does not go through handleAuthIssue's own
  expires_in_seconds default, since validateClaims is not the thing enforcing that default —
  nothing prevents a hand-crafted claims token bypassing handleAuthIssue's ExpiresIn logic
  altogether, per AUTH-2).
- Impact: combined with AUTH-2, an attacker who can compute the HMAC can mint a token that
  never expires and is unrevocable (AUTH-1), for any tenant.
- Confidence: Confirmed.

[AUTH-4] HIGH — Externally-reachable mcp-gateway is authenticated with the identical
fleet-wide shared credential as internal service-to-service calls, and documentation
explicitly instructs distributing that credential to multiple third-party CLI agents on
developer machines.
- Location: services/mcp-gateway/cmd/server/main.go:115
  (`auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())` — same Config type, same
  validation code as every internal service); docs/agent-connection-guide.md:48-59 (per-machine
  `~/.hermes/scos.env`, mode 600, holding
  `SCOS_HERMES_BEARER_TOKEN=<internal service token or claims JWT>`) and lines 76-93
  (Claude Code, and by the doc's own framing every other listed agent — Codex, Kilo,
  OpenCode, OpenClaw, Qwen, hermes-agent — reads the same file/credential via
  scos-mcp-bridge.py).
- Evidence: the doc's own comment literally offers "internal service token" as one of the two
  acceptable values for the credential handed to external agent processes, with no separate,
  narrower-scoped credential type defined anywhere in the auth code for
  externally-facing vs. internal-only callers. mcp-gateway's copyIdentityHeaders
  (main.go:780-792) forwards the caller-supplied Authorization header byte-for-byte to every
  downstream service it proxies to, so whatever trust that bearer carries at the gateway is
  carried unchanged into the entire internal mesh.
- Reachable entry point: POST /mcp on mcp-gateway (documented as network-reachable via
  Tailscale in docs/agent-connection-guide.md:4).
- Attacker preconditions: read access to any one of potentially several developer machines'
  ~/.hermes/scos.env (mode 600 mitigates same-machine other-user reads, but does not protect
  against malware, a compromised CLI agent/extension, misconfigured backup, or a machine-level
  compromise), OR compromise of any of the several third-party agent CLIs (Codex, Kilo,
  OpenCode, OpenClaw, Qwen) that this file is explicitly shared with.
- Impact: per AUTH-1, that credential cannot be revoked without redeploying every internal
  service; per AUTH-2/AUTH-3 the claims-JWT alternative is only marginally safer
  (unrevocable, and unbounded lifetime if exp is omitted). Leakage from the *least* trusted
  point in the whole system (an individual developer's laptop running third-party CLI tools)
  yields the *same* level of access as any internal service.
- Confidence: High-confidence (the code-side blast radius is Confirmed; whether operators
  actually choose the raw internal-service-token option vs. a narrowly-scoped claims JWT in
  practice is a deployment choice the doc explicitly permits but this pass could not observe
  a live deployment to confirm which option is in use).

Suspicions requiring validation:
- Whether any code path lets an external mcp-gateway caller reach security-gate's
  X-SCOS-Issuer-Secret-gated /v1/auth/issue despite copyIdentityHeaders not forwarding that
  header — traced and found NOT reachable (copyIdentityHeaders only forwards Authorization,
  X-SCOS-Tenant-ID, X-SCOS-Principal-ID, Idempotency-Key; X-SCOS-Issuer-Secret is absent from
  that list and there is no alternate way for a JSON `args` field to become a header in
  forwardPOST/forwardWithBody). Downgraded to Non-issue for this specific path.
- Whether async workers (outbox processors, queue consumers) re-derive tenant context from a
  possibly-stale or attacker-influenced source rather than re-validating a live claims token
  — a single spot check (services/memory-gateway/cmd/server/outbox.go) shows tenant_id is
  read from an already-resolved requestContext (rc.TenantID) rather than a raw header, which
  is consistent with safe propagation, but this was not traced end-to-end into the
  originating HTTP handler for every one of the ~20 services and is listed as a coverage gap.
- Whether transparency-projection's independent use of SCOS_SECURITY_ISSUER_SECRET
  (services/transparency-projection/cmd/server/main.go) does anything beyond the standard
  AuthenticatedHandler wiring already covered by AUTH-2 — not separately read in this pass.

Tests/commands executed:
- Read (full file): packages/auth/middleware.go, packages/auth/middleware_test.go,
  services/security-gate/cmd/server/main.go, services/security-gate/cmd/server/store.go
- Read (partial, targeted): services/mcp-gateway/cmd/server/main.go (top-level wiring,
  handleMCP, handleToolsCall security-gate dispatch block, forward* helpers,
  copyIdentityHeaders, resolveIdentity), deploy/compose/docker-compose.yml (lines 1-40),
  docs/agent-connection-guide.md (lines 1-100), services/memory-gateway/cmd/server/outbox.go
  (grep + spot read)
- `grep -rl "SCOS_SECURITY_ISSUER_SECRET" services/ packages/` — confirmed only three Go
  source locations reference the var by name (security-gate, transparency-projection,
  packages/auth), contrasted against the compose anchor to establish the value is injected
  fleet-wide regardless of source-level reference count
- `grep -rn "SCOS_SECURITY_ISSUER_SECRET\|SCOS_INTERNAL_SERVICE_TOKEN" --include=*.yml
  --include=*.yaml --include=*.env* -r .` — located deploy/compose/docker-compose.yml,
  deploy/native/env/scos-fabric.env.example, .env.example as the distribution points
- `grep -rn "SCOS_INTERNAL_SERVICE_TOKEN\|revoke" packages/auth/ services/security-gate/`
  — confirmed the only "revoke" code in the auth surface is security-gate's DB-backed
  security_tokens system, unrelated to SCOS_INTERNAL_SERVICE_TOKEN
- `grep -n "func (s *gatewayServer) copyIdentityHeaders" -A 25
  services/mcp-gateway/cmd/server/main.go` — confirmed header forwarding list
- `find packages/auth -type f`, `ls services/`, `find services/security-gate -type f` —
  inventory

Coverage gaps:
- Did not read the remaining ~17 services individually beyond grep confirmation that they
  consume the same packages/auth.AuthenticatedHandler; assumed uniform wiring based on the
  compose anchor and the pattern observed in security-gate and mcp-gateway. A service that
  deviates from this pattern (e.g. a custom auth check) was not ruled out for every service.
- Did not trace async/queue/outbox identity propagation end-to-end for every service —
  only memory-gateway's outbox.go was spot-checked.
- Did not review database/policies/ RLS SQL directly in this pass (relied on the established
  background context plus setPrincipalContext call sites); did not independently verify RLS
  policy definitions correctly scope every table by scos.tenant_id.
- Did not review MFA, session/cookie-based login, account recovery, or OAuth/OIDC flows —
  grep and file inventory found no such code in this repository; the identity model here is
  entirely service-credential / claims-token based with no end-user login surface observed.
  This should be independently confirmed rather than treated as settled, since it was not
  the primary focus of this pass.
- Did not confirm in a live/staging deployment which credential type
  (raw internal-service-token vs. claims JWT) operators actually place in
  ~/.hermes/scos.env for AUTH-4 — code-level blast radius is confirmed, live practice is not.
- Did not review services/tool-governance or services/execution-proxy internals in depth
  (referenced only via cross-file grep and background context) for their own
  identity-propagation correctness beyond confirming they sit behind the same
  AuthenticatedHandler.

Areas explicitly not reviewed:
- Authorization/permission-scoping logic beyond what overlaps identity (deferred to
  hostile-authorization-auditor's scope)
- Non-auth business logic of security-gate's secret-resolution/Securestore integration
  beyond how it re-uses the identity established by AuthenticatedHandler
- Frontend/dashboard code (services/transparency-projection's UI, if any) and any
  browser-facing session/cookie handling

Recommended follow-up:
1. Introduce a revocable, DB-backed (or centrally-cached deny-list) check for
   SCOS_INTERNAL_SERVICE_TOKEN and for claims-token jti, consulted by
   packages/auth.AuthenticatedHandler on every request, so a compromised credential can be
   killed without a fleet-wide redeploy (addresses AUTH-1).
2. Stop broadcasting SCOS_SECURITY_ISSUER_SECRET to every service via the shared compose
   anchor; restrict claims-token *verification* to a narrow, well-audited allowlist of
   services that actually need it (or move to asymmetric signing so verifiers cannot mint),
   and bind each minted token to an intended-audience claim checked by every verifier
   (addresses AUTH-2).
3. Make `exp` a mandatory claim in validateClaims — reject any claims token where
   raw.Exp <= 0 rather than treating it as no-expiry (addresses AUTH-3).
4. Define a distinct, narrower-scoped credential type for externally-reachable mcp-gateway
   callers (e.g. per-agent claims tokens with short TTL and a real revocation list per
   recommendation 1), and update docs/agent-connection-guide.md to stop offering the raw
   internal-service-token as an acceptable value for SCOS_HERMES_BEARER_TOKEN on external
   agent machines (addresses AUTH-4).
5. Extend this pass to the remaining services not individually read, and to end-to-end
   async/outbox identity propagation, to close the coverage gaps above.
```

## Cloud control-plane follow-up (2026-08-28)

This follow-up preserves the prior audit above and adds the new cloud account/admin scope. Verified with `go test ./...`, `go build ./...`, Compose config validation, and `git diff --check`.
