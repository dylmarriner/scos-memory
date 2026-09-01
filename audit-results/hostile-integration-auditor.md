# Specialist audit artifact

```text
Skill: hostile-integration-auditor
Agent: hostile-integration-auditor-workflow-agent
Scope: services/extension-manager hook dispatch (signature verification, replay protection,
  retry/duplicate delivery); services/hermes-provider and services/graphify-provider
  external-facing integration points; webhook-like callback paths and their trust
  assumptions about the caller. Extended to services/mcp-gateway because it is the
  upstream caller for every graphify-provider and extension-manager endpoint reachable
  from an external MCP client, and its identity-resolution code is load-bearing for the
  trust boundary this skill is scoped to audit.

Files/components inspected:
  - services/extension-manager/cmd/server/main.go (1124 lines, full read)
  - services/extension-manager/cmd/server/canary.go (334 lines, full read)
  - services/extension-manager/cmd/server/main_test.go (hook-dispatch tests, grepped)
  - services/hermes-provider/cmd/server/main.go (756 lines, full read)
  - services/hermes-provider/cmd/server/lifecycle.go (376 lines, full read)
  - services/hermes-provider/cmd/server/store.go (485 lines, full read)
  - services/graphify-provider/cmd/server/graphify.go (111 lines, full read)
  - services/graphify-provider/cmd/server/main.go (299 lines, full read)
  - services/mcp-gateway/cmd/server/main.go (dispatchTool, forward*, resolveIdentity,
    copyIdentityHeaders, handleMCP/handleToolsCall — read in full via targeted offsets)
  - packages/auth/middleware.go (232 lines, full read) — AuthenticatedHandler,
    ControlPlaneHandler, validateClaims, Config/FromEnvironment
  - docs/agent-connection-guide.md (grepped for bearer-token provisioning guidance)

Entry points inspected:
  - POST /v1/hooks/dispatch (extension-manager) — internal hook fan-out trigger
  - POST /v1/install, /v1/install/dryrun, /v1/extensions/{id}/canary[/resolve|/promote|/rollback]
    (extension-manager) — manifest install/canary paths that register hook endpoints
  - POST /mcp (tools/call, method scos_graphify_query/_path/_explain, scos_* memory tools)
    (mcp-gateway) — the externally-reachable JSON-RPC/MCP endpoint per
    docs/agent-connection-guide.md
  - POST /v1/graphify/query, /v1/graphify/path, /v1/graphify/explain, /v1/graphify/extract
    (graphify-provider) — CLI-backed graph query/build surface
  - POST /v1/provider/prefetch, /sync_turn, /session_switch, /pre_compress, /memory_write,
    /delegation, /queue_prefetch, /turn_start, /session_end, /tool_call, /save_config
    (hermes-provider) — full provider lifecycle surface

Important callers/callees:
  - mcp-gateway.dispatchTool → graphify-provider /v1/graphify/{query,path,explain} (forwardPOST,
    forwards Authorization/X-SCOS-Tenant-ID/X-SCOS-Principal-ID/Idempotency-Key headers verbatim
    via copyIdentityHeaders)
  - graphify-provider.handle{Query,Path,Explain} → os/exec.CommandContext("graphify", args...)
    (argv slice, not shell — see graphify.go:19)
  - graphify-provider.handleExtract → runGraphify → loadGraph → postJSON →
    memory-intelligence /v1/facts, /v1/relations (server's own configured tenant/principal,
    not caller-supplied — see main.go:71-78, 96-97)
  - extension-manager.dispatchHooks → http.Client.Do(hook.Endpoint) with
    Authorization: Bearer $SCOS_INTERNAL_SERVICE_TOKEN, X-SCOS-Tenant-ID,
    X-SCOS-Principal-ID (main.go:733-746) — hook.Endpoint comes from the installed
    extension's own manifest (HookEndpoints map), gated only at install/canary time by
    verifySignature (ed25519) and validateManifest
  - packages/auth.AuthenticatedHandler wraps every service's mux (extension-manager,
    hermes-provider, graphify-provider, mcp-gateway all confirmed) and is the sole gate
    between "no Authorization header" and every downstream handler

Workflows traced:
  1. Extension install → hook registration → hook dispatch → outbound webhook call to
     extension-declared endpoint, including SSRF/URL-scheme validation, circuit breaker,
     fallback policy, and credential attachment (main.go:701-775).
  2. External MCP client → mcp-gateway /mcp tools/call → identity resolution (claims vs.
     header fallback) → forwardPOST with copied headers → graphify-provider CLI exec.
  3. hermes-provider request identity resolution (resolveIdentity) → RLS principal context
     (set_config) → idempotent write (sync_turn) → event append, for comparison against
     extension-manager's and mcp-gateway's identity handling.
  4. Canary manifest install path (canaryStart) to confirm it reuses validateInstallManifest
     (same signature gate) rather than being a side door around signature verification.

Trust boundaries:
  - External MCP client (Claude Code/Desktop) → mcp-gateway (port 8098, externally reachable
    per docs/agent-connection-guide.md). Documented bearer provisioning explicitly allows
    "internal service token or claims JWT" (docs/agent-connection-guide.md:52) as the value
    handed to an external agent's bridge config.
  - mcp-gateway → graphify-provider / extension-manager / hermes-provider / memory-intelligence
    (internal, TLS, shared bearer or forwarded claims).
  - extension-manager (trusted core service, holds SCOS_INTERNAL_SERVICE_TOKEN) → arbitrary
    HTTPS endpoint declared by an installed extension's manifest (extension is a lower-trust,
    signature-gated but still external-content boundary; §22.4 isolation modes exist
    specifically because extension code is not fully trusted).
  - graphify-provider → local `graphify` CLI subprocess, invoked with caller-influenced
    argv strings (q/from/to/node) originating, via mcp-gateway, from an external MCP client.

Attacker-controlled inputs:
  - hookDispatchRequest.TenantID / .PrincipalID (extension-manager /v1/hooks/dispatch body) —
    fully caller-supplied, no cross-check against any authenticated claim.
  - X-SCOS-Tenant-ID / X-SCOS-Principal-ID request headers presented to mcp-gateway when the
    caller authenticates with the raw shared bearer token (no JWT claims) — used verbatim as
    the resolved identity and then forwarded downstream.
  - ExtensionManifest.HookEndpoints[event] — attacker-controlled to the extent a publisher
    holding (or having leaked) the ed25519 signing key controls it; validated only for
    scheme (https, or http+loopback), not for destination host/network.
  - q / from / to / node fields in graphify-provider's /v1/graphify/{query,path,explain}
    request bodies — sourced from external MCP tool call `arguments`, forwarded unmodified by
    mcp-gateway, passed as CLI argv to the `graphify` binary with no length bound, character
    allow-list, or leading-dash/flag-injection guard.

Invariants tested:
  - "A hook dispatch acts on behalf of the tenant/principal the caller is authenticated as,
    not whatever the caller claims in the JSON body" — VIOLATED for extension-manager
    (Finding 1).
  - "A caller without tenant/principal claims cannot assert an arbitrary tenant identity via
    request headers" — VIOLATED for mcp-gateway's claims-absent fallback path when the
    documented raw-token bearer mode is used (Finding 2); confirmed as the same pattern,
    deliberately, in hermes-provider and (transitively) memory-intelligence, so this is a
    fleet-wide design choice, not an isolated bug — but mcp-gateway is the one instance of
    this pattern that is documented as reachable from outside the deployment.
  - "The shared internal-service credential is only sent to endpoints inside the trust
    boundary it authorizes" — VIOLATED for extension-manager's hook dispatch (Finding 3):
    any HTTPS host, not just internal cluster hosts, is accepted.
  - "External, low-trust input never reaches an exec.Command argv without being either
    shell-safe (already true here) and defanged against being interpreted as a flag" —
    not fully upheld for graphify-provider (Finding 4); shell injection itself is correctly
    prevented (argv slice, not a shell string).
  - Idempotency / replay protection on hermes-provider's mutating endpoint (sync_turn) —
    HELD: request-hash-checked idempotency_keys table, 24h expiry, correct duplicate replay
    semantics (store.go:76-107; lifecycle.go's queue endpoints are advisory/pre-warm only and
    do not need idempotency since they are not persisted mutations).
  - Manifest signature verification (ed25519) gates both /v1/install and the canary start
    path identically — HELD; canary is not a side door around the signature gate
    (canary.go:205 reuses validateInstallManifest verbatim).

Findings:
  1. [High-confidence] extension-manager /v1/hooks/dispatch (main.go:649-699, 701-775) never
     reads auth.CanonicalClaims from the request context and never compares the request
     body's TenantID/PrincipalID against any authenticated identity. Any caller holding
     valid credentials for this endpoint (shared token, or a claims-bearing JWT for *any*
     tenant, since claims are never consulted at all here) can set tenant_id/principal_id
     to an arbitrary value; those values are forwarded verbatim as X-SCOS-Tenant-ID /
     X-SCOS-Principal-ID headers to every subscribed extension's hook endpoint
     (main.go:736-737). This is a real internal-consistency gap: hermes-provider
     (main.go:110-138), mcp-gateway (main.go:797-807), and (per grep) memory-intelligence,
     agent-registry, security-gate, autonomous-control-plane, compatibility-gateway and
     tool-governance all call auth.CanonicalClaims and bind body-supplied tenant/principal
     to it; extension-manager as a whole never imports this check anywhere
     (`grep CanonicalClaims services/extension-manager` — no hits). No test exercises
     claims-binding for hook dispatch; TestHookDispatchInvokesEndpointAndPropagatesIdentity
     (main_test.go:49) only asserts that whatever identity is given in the body is
     *propagated*, which is the behavior in question.
     Impact: a compromised or merely misbehaving internal caller (or an agent whose JWT
     scopes it should have limited to one tenant, if extension-manager is ever reached by
     less-trusted internal callers than assumed) can trigger tenant-A's hook subscribers
     while asserting tenant-B's identity, causing extension endpoints to act, log, or make
     decisions under a spoofed tenant/principal.

  2. [Plausible] mcp-gateway's resolveIdentity (main.go:797-807) — the sole identity source
     for the externally-reachable /mcp endpoint — falls back to trusting the caller-supplied
     X-SCOS-Tenant-ID / X-SCOS-Principal-ID headers whenever the presented bearer token does
     not carry parseable JWT claims (i.e. whenever the caller authenticates with the raw
     SCOS_INTERNAL_SERVICE_TOKEN rather than a per-tenant claims token). docs/agent-connection-
     guide.md:52 explicitly documents `SCOS_HERMES_BEARER_TOKEN=<internal service token or
     claims JWT>` as a supported way to configure an external Claude Code/Desktop bridge,
     meaning the raw shared token is a documented, supported credential for external callers
     of the one service in this fleet explicitly described as externally reachable
     (services/mcp-gateway, port 8098, per the known context this audit was scoped from).
     Any holder of that shared token can set arbitrary X-SCOS-Tenant-ID/X-SCOS-Principal-ID
     and impersonate any tenant/principal for every mcp-gateway-forwarded tool call
     (scos_recall, scos_correct, scos_forget, scos_forget_all, scos_code_*, scos_graphify_*,
     etc. — see dispatchTool, main.go:227-330+). This mirrors the same fallback pattern
     already present in hermes-provider and memory-intelligence, so it is very likely an
     intentional fleet-wide trust model (the raw token is meant to denote a fully-trusted
     internal caller) rather than an oversight local to mcp-gateway — hence Plausible rather
     than Confirmed/High-confidence: I could not find the deployment/provisioning policy that
     would confirm whether the raw token is ever handed to a genuinely external, lower-trust
     party in production, only that the docs describe doing so as supported. Downgrade to
     Non-issue if the token is treated organization-wide as equivalent to root/service-account
     trust and is provisioned 1:1 per tenant deployment rather than shared across tenants.

  3. [High-confidence] extension-manager's dispatchHooks (main.go:720-723) validates a
     hook endpoint URL only by scheme — `https` unconditionally, or `http` restricted to
     127.0.0.1/localhost — with no restriction on the *host* for the https case. Any
     installed extension (gated only by a valid ed25519 manifest signature, not by any
     network allow-list) can declare `hook_endpoints[event] = "https://<any host
     whatsoever>/..."`, and dispatchHooks will send that host the
     `SCOS_INTERNAL_SERVICE_TOKEN` in an `Authorization: Bearer` header
     (main.go:738-740) plus the full event payload (X-SCOS-Tenant-ID, X-SCOS-Principal-ID,
     and the caller-supplied payload map) whenever the subscribed hook event fires. This
     both (a) exfiltrates the shared internal-service credential to a destination outside
     the internal network if the declared host is external, and (b) allows a malicious or
     compromised (but validly signed) extension to use extension-manager as a confused
     deputy to reach *other* internal services over HTTPS with that credential attached
     (SSRF against internal-only APIs), which is a materially larger blast radius than the
     extension's own declared isolation mode (`separate_process` / `container` /
     `wasm_sandbox` / `remote_endpoint`, main.go:1079-1093) would suggest, since none of
     those isolation modes constrain what URL extension-manager itself is willing to POST
     the shared credential to.

  4. [Plausible] graphify-provider's handleQuery/handlePath/handleExplain
     (main.go:239-298) take caller-supplied strings (q, from, to, node) — which, per the
     mcp-gateway trace above, can originate from an external MCP client's tool-call
     arguments — and pass them as positional CLI arguments to `exec.CommandContext(ctx,
     "graphify", args...)` (graphify.go:15-26). Shell injection is correctly prevented
     (args is a Go slice, not a shell string), and the comment at graphify.go:16-17
     explicitly documents this rationale. However, no argument is checked for a leading
     `-`/`--`, bounded in length, or otherwise restricted before being handed to the
     third-party `graphify` binary; if that CLI's argument parser treats a leading-dash
     value as a flag (a common behavior for CLI frameworks, e.g. Cobra/argparse-style
     tools) rather than a positional argument, a crafted `q`/`from`/`to`/`node` value could
     trigger unintended `graphify` flags (config file overrides, output-path redirection,
     etc.) rather than a query. I did not have access to the `graphify` binary's actual
     flag surface to confirm exploitability, hence Plausible rather than Confirmed. Also
     note handleQuery/Path/Explain use `json.NewDecoder(r.Body).Decode(...)` with no
     `http.MaxBytesReader` bound (unlike extension-manager, which consistently applies
     MaxBytesReader on every decode path), so an arbitrarily large request body is fully
     buffered before the 200-byte-ish check fails — a minor amplification/DoS surface on
     top of the argument-injection concern.

  5. [Non-issue] graphify-provider's handleExtract does not accept caller-supplied
     tenant/principal/namespace at all — it always uses the server's own
     GRAPHIFY_TENANT_ID/GRAPHIFY_PRINCIPAL_ID/GRAPHIFY_NAMESPACE_ID env vars
     (main.go:120-123, graphify.go:96-97), so there is no tenant-spoofing vector on the
     ingestion path itself (checked because it looked at first glance like another
     tenant-binding gap, but the identity is not caller-influenced here at all).

Suspicions requiring validation:
  - Whether `SCOS_INTERNAL_SERVICE_TOKEN` is provisioned per-tenant-deployment (single
    tenant per token) or shared across multiple tenants behind one mcp-gateway instance in
    the real deployment topology — this determines whether Finding 2 is a real cross-tenant
    vulnerability or an accepted "root credential" trust model. Not resolvable from source
    alone; would need deployment/ops configuration (docs/deploy or infra-as-code) review.
  - Whether the `graphify` CLI's argument parser is vulnerable to flag injection from a
    leading-dash positional argument (Finding 4) — would need the vendored/installed
    `graphify` binary's own source or `--help` output, which is outside this repository
    (github.com/Graphify-Labs/graphify per the comment at graphify.go:22).
  - Whether extension publishers who hold the ed25519 signing key are trusted to declare
    arbitrary external hook endpoints as a matter of product design (i.e. Finding 3 is
    "accepted risk of a compromised/malicious but signed publisher") — GOVERNANCE.md §22.4
    is referenced for isolation modes but I did not have that document in scope to confirm
    whether it discusses hook-endpoint network restrictions.

Tests/commands executed:
  - grep -rln "hook" services/extension-manager --include="*.go" -i
  - grep -rln "CanonicalClaims" services  (enumerated every service that binds
    body-supplied identity to authenticated claims, to establish the fleet-wide
    convention extension-manager departs from)
  - grep -n "func resolveIdentity|forwardPOST|forwardGET|X-SCOS-Agent-Name|
    desktopAgentToolNames" services/mcp-gateway/cmd/server/main.go
  - grep -n "graphify|scos_graphify|graphify-provider" services/mcp-gateway/cmd/server/*.go
  - grep -n "Authorization|Bearer|SCOS_INTERNAL_SERVICE_TOKEN|JWT|token"
    docs/agent-connection-guide.md
  - grep -n "hookDispatch|dispatchHooks|tenant_id.*claims" services/extension-manager/cmd/
    server/main_test.go (confirmed no claims-binding test exists for hook dispatch)
  - No code was modified; no build/test suite was executed (read-only source audit).

Coverage gaps:
  - Did not read services/extension-manager/cmd/server/canary_test.go,
    services/hermes-provider/cmd/server/{main_test.go,lifecycle_test.go,readiness_test.go},
    services/graphify-provider/cmd/server/graphify_test.go, or
    services/extension-manager/cmd/server/{main_test.go beyond the grepped hook sections,
    readiness_test.go} in full — only grepped for the specific claims/identity assertions
    relevant to Findings 1 and 3.
  - Did not inspect memory-intelligence's /v1/facts, /v1/relations handlers (the actual
    ingestion sink graphify-provider posts to) for their own tenant/RLS enforcement — only
    confirmed graphify-provider sends server-configured (not caller-controlled) identity to
    them.
  - Did not inspect execution-proxy or tool-governance, named in the master task's known
    context as the shell-execution and gating services, since they were outside this
    skill's assigned scope (extension-manager hook dispatch, hermes-provider,
    graphify-provider, webhook-like callback paths) — flagging for the hostile-control-flow
    or hostile-authorization specialist if not already covered.
  - Did not review GOVERNANCE.md §22.3/§22.4/§22.6/§22.7, which extension-manager's
    comments cite extensively, so I could not confirm whether Finding 3's hook-endpoint
    host restriction (or lack thereof) contradicts an explicit governance requirement, only
    that it contradicts the narrower SSRF-prevention intent visible in the code itself
    (main.go:721, "must use HTTPS or loopback HTTP").
  - Did not attempt to reach a running instance of any service (no docker-compose / test
    harness was started); all findings are static-analysis based, traced to exact
    file:line evidence, not dynamically confirmed against a live request/response.
  - Did not review database/policies/ RLS definitions in this pass (covered by report,
    referenced as known context, not independently re-verified here) beyond confirming
    hermes-provider calls set_config('scos.tenant_id', ...) with the resolved (claims- or
    header-bound) identity before every mutating query.

Areas explicitly not reviewed:
  - services/execution-proxy, services/tool-governance (out of this skill's assigned scope;
    named in the task's "known context" as another specialist's likely territory)
  - services/memory-intelligence, services/code-intelligence handler implementations
    (only their role as callees of graphify-provider/mcp-gateway was traced)
  - Frontend/dashboard code, CI/CD, and non-Go adapters (packages/ outside packages/auth)
  - GOVERNANCE.md itself (referenced only via in-code comments, not opened directly)

Recommended follow-up:
  1. extension-manager /v1/hooks/dispatch: bind hookDispatchRequest.TenantID/PrincipalID to
     auth.CanonicalClaims the same way hermes-provider's resolveIdentity does (return 403 on
     mismatch when claims are present) before this endpoint is trusted to run in any
     multi-tenant deployment.
  2. extension-manager dispatchHooks: restrict hook.Endpoint to an explicit internal-host
     allow-list (or require a separate, extension-scoped credential rather than forwarding
     SCOS_INTERNAL_SERVICE_TOKEN) so a signed-but-malicious extension cannot use
     extension-manager as an SSRF pivot or exfiltrate the shared internal credential to an
     arbitrary HTTPS host.
  3. Confirm with the deployment/ops owner whether SCOS_INTERNAL_SERVICE_TOKEN is ever
     shared across multiple tenants behind one mcp-gateway instance; if so, either stop
     documenting the raw token as a supported external-agent bearer credential (docs/agent-
     connection-guide.md:52) or add tenant-header validation ahead of the CanonicalClaims
     fallback in mcp-gateway's resolveIdentity.
  4. graphify-provider: reject query/from/to/node values that begin with `-` (or otherwise
     insert a `--` argv separator before the user-supplied value) before invoking the
     `graphify` CLI, and add http.MaxBytesReader to its request decoders to match
     extension-manager's convention.
  5. Hand Findings 1-3 to the hostile-authorization-auditor / hostile-exploit-chain-auditor
     for cross-tenant and confused-deputy impact confirmation, since both compound with
     each other (a caller that can spoof tenant_id via Finding 1/2 and a hook endpoint that
     can reach arbitrary internal hosts via Finding 3 together describe a plausible
     cross-tenant-to-lateral-movement chain, even though each individually is scored below
     Critical here pending that cross-review).
```
