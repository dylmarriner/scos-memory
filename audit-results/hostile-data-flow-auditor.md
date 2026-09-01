# Specialist audit artifact

```text
Skill: hostile-data-flow-auditor
Agent: hostile-data-flow-auditor-workflow-agent
Scope: Trace attacker-controlled input (headers, request bodies, env, webhook/extension payloads)
  through services/extension-manager, services/graphify-provider, services/code-intelligence,
  services/context-optimisation to storage/exec/network sinks, including second-order
  (stored-then-later-used) paths. Extended upstream one hop into services/mcp-gateway where the
  scoped services' trust boundary (X-SCOS-Tenant-ID / X-SCOS-Principal-ID) originates, per the
  skill's "compare application checks with adapters... follow identity, tenant, state, and
  attacker data across every boundary" requirement.

Files/components inspected:
  - services/extension-manager/cmd/server/main.go (1124 lines) - manifest install, hook
    registry, hook dispatch, capabilities, PATCH/DELETE extension mutation
  - services/extension-manager/cmd/server/canary.go (333 lines) - canary install/promote/rollback
  - services/graphify-provider/cmd/server/main.go (298 lines) - extract/query/path/explain HTTP
    handlers
  - services/graphify-provider/cmd/server/graphify.go (110 lines) - CLI exec wrapper, graph.json
    parsing, postJSON to memory-intelligence
  - services/code-intelligence/cmd/server/main.go (1565 lines) - repo/index/query/context/impact
    endpoints, in-memory + on-disk store, symbol extraction
  - services/context-optimisation/cmd/server/main.go (1040 lines, read through line ~940) -
    input/output optimise, capsule compile/evaluate/restore, file-backed CAS source store
  - services/mcp-gateway/cmd/server/main.go (read in full for identity-header flow: lines
    1-260, 646-860, 1330-1480) - external MCP endpoint, identity resolution, header forwarding,
    tool catalogue for the four scoped services
  - packages/auth/middleware.go (231 lines) - AuthenticatedHandler, ControlPlaneHandler,
    claims validation
  - Not fully read: services/context-optimisation/cmd/server/main.go lines 940-1040 (tail of
    compression/allocation helpers), *_test.go files in all four services (used only to confirm
    expected behavior where cited), packages/tlsserve/serve.go (assumed to provide mTLS/TLS
    transport only, not an additional authorization layer, based on its name and usage sites)

Entry points inspected:
  - POST /mcp (services/mcp-gateway) -> tools/call -> dispatchTool() -> forwardGET/POST/PATCH/
    DELETE to extension-manager, graphify-provider, code-intelligence, context-optimisation
    (this is the externally-reachable ingress per docs/agent-connection-guide.md, port 8098)
  - extension-manager: POST /v1/install, /v1/install/dryrun, /v1/extensions/{id}/canary(/resolve
    |/promote|/rollback), POST /v1/hooks/dispatch, PATCH /v1/extensions/{id}, POST /v1/skills
  - graphify-provider: POST /v1/graphify/{extract,query,path,explain}
  - code-intelligence: POST /v1/repos, /v1/index, /v1/query, /v1/context, /v1/impact,
    /v1/impact/reconcile; GET /v1/repos, /v1/repos/{id}
  - context-optimisation: POST /v1/optimize/{input,output}, /v1/context/{compile,evaluate,
    restore}

Important callers/callees:
  - mcp-gateway.dispatchTool -> copyIdentityHeaders (copies Authorization, X-SCOS-Tenant-ID,
    X-SCOS-Principal-ID, Idempotency-Key verbatim from the inbound client request) -> code-
    intelligence.requireTenant / extension-manager (no tenant check) / context-optimisation
    (no tenant check at all)
  - extension-manager.dispatchHooks -> http.Client.Do against manifest-declared hook.Endpoint,
    with Authorization: Bearer $SCOS_INTERNAL_SERVICE_TOKEN attached
  - extension-manager.handleInstall/canaryStart -> validateInstallManifest -> verifySignature
    (ed25519, SCOS_EXTENSION_SIGNING_PUBLIC_KEY) gates all manifest-controlled fields, including
    HookEndpoints
  - graphify-provider.handleExtract -> runExtract (exec.Command, no shell) -> loadGraph
    (os.ReadFile + json.Unmarshal of graphify's own output) -> ingestNode/ingestEdge -> postJSON
    -> memory-intelligence /v1/facts, /v1/relations (second-order: graph.json content becomes
    Fact.content verbatim, stored, and later served back through scos_recall/scos_explain)
  - graphify-provider.handleQuery/handlePath/handleExplain -> runGraphify (exec.CommandContext,
    args slice, no shell) with client-supplied q/from/to/node forwarded as raw positional CLI
    arguments to the external `graphify` binary
  - code-intelligence.handleIndex -> extractSymbols -> extractGoSymbols/extractPythonSymbols/
    extractTSJSSymbols/extractConfigurationSymbols -> candidate.symbols/chunks (all keyed by
    symbolScope = sha256(repoID + path), so cross-repo collisions are not possible)
  - context-optimisation.handleInputOptimise -> buildCapsule -> authoritativeSourceStore.Put
    -> fileSourceStore.path (filepath.Join(root, objectID+"-"+version+".source")) -> os.WriteFile
  - context-optimisation.handleContextRestore -> validSourceRef (rejects "/" and "\" in
    objectID/version) -> authoritativeSourceStore.Get -> os.ReadFile

Workflows traced:
  1. External MCP client -> mcp-gateway /mcp tools/call -> code-intelligence /v1/query
     (tenant scoping via X-SCOS-Tenant-ID header, traced end to end)
  2. External MCP client -> mcp-gateway -> extension-manager /v1/install (signed manifest) ->
     registerExtensionLocked -> hooks map -> later /v1/hooks/dispatch -> outbound HTTPS call
     carrying SCOS_INTERNAL_SERVICE_TOKEN (second-order: install-time data controls a later,
     separately-triggered egress call)
  3. External MCP client -> mcp-gateway -> graphify-provider /v1/graphify/extract -> `graphify`
     CLI -> graph.json on local disk -> re-read and parsed -> POSTed to memory-intelligence as
     Facts (second-order: file written by one process, trusted and parsed by another, then
     persisted as long-lived memory content)
  4. External MCP client -> mcp-gateway -> context-optimisation /v1/optimize/input -> CAS write
     -> later /v1/context/restore -> CAS read (second-order: content written by one caller,
     retrievable by a different caller who can supply the matching capsule)

Trust boundaries:
  - External network -> mcp-gateway (bearer token or claims JWT; the only boundary that
    authenticates an external human/agent identity)
  - mcp-gateway -> {extension-manager, graphify-provider, code-intelligence,
    context-optimisation} (SCOS_INTERNAL_SERVICE_TOKEN plus forwarded X-SCOS-Tenant-ID /
    X-SCOS-Principal-ID headers; code-intelligence additionally requires
    X-SCOS-Control-Plane-Token via auth.ControlPlaneHandler)
  - extension-manager -> extension-owned hook.Endpoint (manifest-declared, signature-gated at
    install time, but the *dispatch* call is not further restricted to internal hosts once
    installed)
  - graphify-provider -> local `graphify` binary (process boundary, args-only, no shell)
  - graphify-provider -> memory-intelligence (SCOS internal peer, identity headers only, no
    bearer token observed on this specific call path -- see Suspicions)
  - context-optimisation -> local filesystem CAS (no tenant boundary enforced inside the
    service at all)

Attacker-controlled inputs:
  - HTTP request headers on the externally-reachable /mcp endpoint: Authorization,
    X-SCOS-Tenant-ID, X-SCOS-Principal-ID (client-settable on the raw net/http.Request before
    any sanitization)
  - MCP tool call arguments (JSON body under params.arguments) for every governed tool,
    including manifest bodies, hook dispatch event/tenant_id/principal_id/payload, graphify
    query/from/to/node strings, and context-optimisation input/capsule fields
  - Extension manifest fields (HookEndpoints, CapabilitiesProvided, SkillsExported,
    Permissions) once a manifest passes signature verification
  - graphify's own graph.json output (nodes/edges), which is trusted as-is and becomes Fact
    content/relations in memory-intelligence

Invariants tested:
  - "A query can never be widened to another tenant's repositories" (code-intelligence
    §19.10 comment, main.go:748-751) -- verified true *within* code-intelligence's own request
    handling, but the tenant value it trusts is sourced from a header that upstream
    (mcp-gateway) does not guarantee reflects the authenticated caller (see Findings #1).
  - "Callers cannot provide replacement content during restoration" (context-optimisation,
    SourceStore doc comment) -- verified true: handleContextRestore never accepts inline
    content, only (objectID, version, contentHash) references, and Put enforces
    sha256(content) == contentHash before every write.
  - "query/path/explain arguments are user-controlled MCP tool input" and therefore passed via
    args slice, never a shell (graphify.go:15-17 comment) -- verified true, exec.CommandContext
    with a fixed binary name and an args slice; no shell metacharacter risk. Flag-injection risk
    (values starting with "--") was not disproved -- see Findings #4.
  - Manifest install is "not a privileged side door" for canaries (canary.go:203-204) --
    verified true, canaryStart calls the same validateInstallManifest/verifySignature gate as
    handleInstall.
  - Index generation commit is atomic / no partial state (code-intelligence §19.3-19.4) --
    verified true: handleIndex builds a full `candidate` store and only swaps the live maps
    after persistLocked() succeeds.

Findings:
  1. [HIGH-CONFIDENCE] Client-supplied X-SCOS-Tenant-ID / X-SCOS-Principal-ID headers on the
     externally-reachable mcp-gateway are forwarded verbatim to every downstream service,
     bypassing the claims-derived identity mcp-gateway itself computes.
     services/mcp-gateway/cmd/server/main.go:227-237 (dispatchTool) only injects the
     claims-resolved identity into these headers "if r.Header.Get(name) == \"\"" -- i.e. only
     when the header is absent. copyIdentityHeaders (main.go:780-792), used by every
     forwardGET/forwardPOST/forwardPATCH/forwardDELETE call, then copies
     src.Header.Get("X-SCOS-Tenant-ID") straight onto the outbound request with no comparison
     against the identity resolveIdentity() (main.go:797-807) actually authenticated. A caller
     who holds a valid claims-bearing bearer token for tenant A can add
     `X-SCOS-Tenant-ID: tenant-B` (and, if desired, `X-SCOS-Principal-ID`) to the same HTTP
     request; mcp-gateway's own authentication (packages/auth/middleware.go
     AuthenticatedHandler) never inspects or strips this header, so it survives untouched into
     dispatchTool. Downstream, code-intelligence's requireTenant()
     (services/code-intelligence/cmd/server/main.go:423-426) reads exactly this header as the
     sole source of tenant scope for /v1/repos, /v1/index, /v1/query, /v1/context, /v1/impact,
     and /v1/impact/reconcile -- there is no cross-check against a validated claim anywhere in
     code-intelligence. Impact: an authenticated caller for one tenant can read another
     tenant's indexed repositories, symbols, and change-impact data (scos_code_repo_list/get,
     scos_code_query, scos_code_impact) and can also register/mutate extension state and hook
     dispatch metadata (extension-manager does not check tenant at all -- see Finding #2)
     purely by setting a header. This directly contradicts the tenant-isolation invariant
     code-intelligence documents at main.go:419-422 and :748-751, because that invariant only
     holds if the header it trusts is itself trustworthy, and mcp-gateway does not make it so.

  2. [HIGH-CONFIDENCE] extension-manager performs no tenant/principal authorization at all on
     any endpoint (services/extension-manager/cmd/server/main.go, entire file) -- every
     extension, skill, hook, receipt, and capability is a single global namespace behind only
     the shared SCOS_INTERNAL_SERVICE_TOKEN / claims bearer. Combined with Finding #1 (mcp-
     gateway forwards a caller-controlled principal/tenant header, and extension-manager does
     not even consult it), any caller reachable through mcp-gateway with the governed
     scos_extension_* tools can enable/disable/rollback/uninstall any tenant's installed
     extensions, read all receipts, and dispatch any hook for any event
     (POST /v1/hooks/dispatch, main.go:662-699) regardless of which tenant installed it. This
     is a data/control-plane cross-tenant issue distinct from #1's read-path leak: it is a
     write-path (PATCH/DELETE/action) issue with no header check present to even attempt to
     bypass.

  3. [PLAUSIBLE] extension-manager's hook dispatch attaches the internal service's own bearer
     credential to any manifest-declared HTTPS endpoint, not just internal SCOS hosts.
     services/extension-manager/cmd/server/main.go:706-767 (dispatchHooks): the only
     restriction on hook.Endpoint is `parsed.Scheme != "https" && !(http+loopback)`
     (main.go:720-723) -- any public HTTPS host is accepted. On every dispatch,
     `hookReq.Header.Set("Authorization", "Bearer "+token)` (main.go:738-740) attaches
     SCOS_INTERNAL_SERVICE_TOKEN -- the same shared secret that authenticates *every* internal
     SCOS service to every other one (packages/auth/middleware.go AuthenticatedHandler,
     `config.Token`) -- to a request destined for an extension-author-controlled server.
     HookEndpoints are gated by ed25519 manifest-signature verification at install time
     (verifySignature, main.go:1025-1042), so this is not open to a fully unauthenticated
     attacker, but it means: (a) any legitimately signed extension (a supply-chain compromise
     of a trusted publisher, or a publisher whose signing key is compromised) receives the
     platform-wide internal service token on every hook firing, which it can use to
     authenticate as "internal-service" against any other SCOS service; and (b) dispatch of a
     given event is externally triggerable by any mcp-gateway caller via scos_hook_dispatch
     (mcp-gateway main.go:459), so the exfiltration is on-demand, not merely incidental. This
     is a significant blast-radius amplifier layered on top of Findings #1/#2: a compromised or
     malicious signed extension effectively obtains the master internal bearer token.

  4. [PLAUSIBLE] graphify-provider forwards client-controlled MCP arguments as raw, unvalidated
     positional CLI arguments to the external `graphify` binary with no rejection of
     flag-like values. services/graphify-provider/cmd/server/main.go:239-298 (handleQuery/
     handlePath/handleExplain) take req.Q / req.From / req.To / req.Node straight from the
     JSON body (itself sourced from externally-supplied MCP tool arguments via mcp-gateway's
     scos_graphify_query/path/explain) and pass them unmodified into
     runGraphify(ctx, repoPath, "query", req.Q) etc. (graphify.go:18-26,
     cmd.CommandContext("graphify", args...)). No check rejects a value beginning with `-` or
     `--`. Whether this is exploitable depends on `graphify`'s own CLI argument parser (not in
     this repo) accepting flags interspersed with subcommand positional arguments; if it does,
     a crafted `q`/`from`/`to`/`node` value could inject a `graphify`-recognized flag (e.g. an
     alternate config/output path or backend selector) rather than being treated as opaque
     search text. Not confirmed against the actual `graphify` binary; flagged as plausible
     flag-injection risk given the untrusted-input comment at graphify.go:15-17 addresses shell
     injection only, not flag injection.

  5. [HARDENING OPPORTUNITY] context-optimisation performs no tenant/principal check on any
     endpoint and its authoritative source store is a single global, unnamespaced
     content-addressable filesystem cache. services/context-optimisation/cmd/server/main.go:
     195-233 (fileSourceStore, defaultSourceStore) and :284-329 (handleContextRestore) key
     stored content only by `objectID = "source-" + sha256(content)[:32]` and version "1"
     (main.go:893-894), with no tenant/principal dimension anywhere in the store key or the
     restore path. Practical exploitability is low because retrieving a given object still
     requires supplying the exact (objectID, version, contentHash) triple, which in practice
     requires already possessing the original content (contentHash is a preimage of it) or
     having seen the capsule mcp-gateway returned to the writer. Flagged as a hardening
     opportunity (defense-in-depth gap, not demonstrated cross-tenant read) rather than a
     confirmed cross-tenant leak, and as an outlier relative to code-intelligence's explicit,
     documented tenant-scoping discipline in the same fleet.

  6. [HARDENING OPPORTUNITY] packages/auth/middleware.go validateClaims (lines 173-226) does
     not require the `exp` claim to be present: if a claims bearer token omits `exp`, no
     expiry check runs at all (the `raw.Exp > 0` guard at line 213 skips the check entirely),
     and the function's own computed `expiresAt` (line 220-223) is never enforced against
     anything -- it is returned in Claims but nothing in this file or the call sites inspected
     (mcp-gateway resolveIdentity) checks Claims.ExpiresAt after validateClaims returns. This
     only matters if the HMAC issuer secret (SCOS_SECURITY_ISSUER_SECRET) is ever exposed or
     an internal issuer is compromised into minting an exp-less token, at which point that
     token would be valid forever. Listed as hardening because forging a valid signature
     without the secret is not itself in scope for a pure data-flow trace.

Suspicions requiring validation:
  - graphify-provider's postJSON to memory-intelligence (graphify.go:86-110) sets only
    X-SCOS-Tenant-ID / X-SCOS-Principal-ID, never an Authorization bearer header, unlike
    mcp-gateway's copyIdentityHeaders which always forwards Authorization. If
    memory-intelligence's own AuthenticatedHandler requires a bearer token on every
    non-health route (as packages/auth/middleware.go suggests it would for any service wired
    through auth.AuthenticatedHandler), then either (a) memory-intelligence is reachable
    without a bearer token from this specific caller pattern -- worth checking whether
    memory-intelligence's mux wraps /v1/facts and /v1/relations in AuthenticatedHandler or
    something looser -- or (b) srv.httpClient (tlsserve.TrustedClient) injects a credential at
    the transport layer (mTLS) that substitutes for the bearer requirement, which would need
    confirming by reading packages/tlsserve/serve.go's TrustedClient implementation (not read
    in this pass). Not resolved; recommend the integration or blind-spot specialist confirm
    which is true, since it changes whether Finding pattern-#1/#2 also reaches
    memory-intelligence through this specific ingestion path.
  - Whether any of the four scoped services are deployed with tlsserve providing mutual TLS
    that independently authenticates the calling service identity (which would partially
    mitigate Findings #1/#2 by requiring network-level compromise of mcp-gateway itself, not
    just a header). packages/tlsserve/serve.go was not read in this pass.
  - Whether governedToolNames()/desktopAgentToolNames in mcp-gateway restrict which external
    agents can reach scos_extension_install, scos_hook_dispatch, scos_code_repo_register, etc.
    in practice (main.go:200-208) -- a narrower agent allowlist would reduce, but not eliminate,
    the population able to trigger Findings #1-#3, since the header-spoofing technique needs
    only a valid bearer/claims token for *some* tenant, not a specific privileged agent name.

Tests/commands executed:
  - Read (via Read tool) of all files listed under "Files/components inspected"; no test binary
    was executed in this pass. `grep -rn` used to locate X-SCOS-Tenant-ID/X-SCOS-Principal-ID
    handling across services/mcp-gateway and services/code-intelligence, and to enumerate
    scos_extension_*/scos_context_*/scos_graphify_* tool wiring in mcp-gateway.
  - Did not run `go build`/`go test` for these packages in this pass; findings are based on
    static trace of the Go source, cross-referenced against the doc comments the code itself
    makes claims in (§19.10, §22.2-22.4 references).

Coverage gaps:
  - context-optimisation/cmd/server/main.go lines ~940-1040 (tail of compression/allocation
    helper functions) not read; unlikely to contain new sinks given the file's structure but
    not confirmed.
  - packages/tlsserve/serve.go not read; TrustedClient/ListenAndServe's actual TLS/mTLS
    semantics are assumed, not verified, and materially affect Findings #1/#2's real-world
    exploitability (see Suspicions).
  - memory-intelligence's own request handling (/v1/facts, /v1/relations, /v1/facts/{id}) was
    not inspected in this pass, even though graphify-provider and mcp-gateway both write/read
    it; the second-order path "graph.json content becomes a stored Fact, later served back
    through scos_recall/scos_explain" (Workflow #3) was traced only as far as the POST call,
    not into how memory-intelligence stores, dedupes, or re-exposes ingested Facts.
  - services/mcp-gateway's governedToolNames()/desktopAgentToolNames restriction logic
    (main.go ~1330-1480, ~200-208) was sampled for the four scoped services' tool names but not
    read exhaustively for every one of the ~15 other backend services it fronts.
  - No dynamic verification (no running services, no crafted HTTP requests fired) was
    performed; all findings are static-trace-derived. Finding #4 (flag injection) in
    particular cannot be confirmed without the actual `graphify` binary's argument parser.
  - Did not examine database/policies/ RLS definitions to see whether Postgres-level tenant_id
    scoping (set_config('scos.tenant_id', ...)) exists as a defense-in-depth backstop for
    code-intelligence or extension-manager's persisted state; code-intelligence's store here is
    in-memory/file-JSON (not Postgres) per codeStore, so RLS would not apply to it regardless.
    extension-manager does use pgxpool/Postgres for its persisted state (main.go:194-204) --
    whether database/policies/ enforces RLS on extension_manager_state was not checked.
  - Did not check whether SCOS_EXTENSION_MANAGER_URL, SCOS_CODE_INTELLIGENCE_URL, etc. are
    network-isolated (e.g. not internet-routable) independent of the mcp-gateway trust
    boundary; the findings assume mcp-gateway is the only externally reachable ingress, per
    the task's stated background context, and were not independently reverified against
    deployment/network configuration.

Areas explicitly not reviewed:
  - services/execution-proxy and services/tool-governance (named in the task's background
    context as executing shell commands / gating execution) were not opened in this pass; the
    task scope named extension-manager, graphify-provider, code-intelligence, and
    context-optimisation specifically, and mcp-gateway was added only as the shared origin of
    the tenant-header trust boundary those four consume.
  - memory-intelligence, hermes-provider, agent-registry, and all other ~11 backend services
    mcp-gateway fronts were not opened, beyond the single grep confirming header-forwarding
    behavior is uniform across dispatchTool's forward* helpers (i.e. the finding is not
    specific to one downstream service's code, it is a property of mcp-gateway's forwarding
    layer itself).
  - Test files (*_test.go) in all four scoped services were not read for coverage-gap analysis
    (that is the hostile-test-gap-auditor's mandate); they were referenced only implicitly when
    inferring intended behavior from doc comments in the corresponding main.go files.

Recommended follow-up:
  - Escalate Findings #1 and #2 to the master auditor / hostile-authorization-auditor as a
    cross-tenant privilege-escalation candidate: mcp-gateway should always overwrite
    X-SCOS-Tenant-ID/X-SCOS-Principal-ID with the identity it resolves from
    auth.CanonicalClaims (or reject the request if a client-supplied value conflicts with the
    authenticated claims), rather than only filling them in when absent
    (services/mcp-gateway/cmd/server/main.go:232-237). extension-manager should additionally
    gain its own tenant-scoping check (mirroring code-intelligence's requireTenant pattern)
    rather than relying entirely on the header mcp-gateway forwards.
  - Escalate Finding #3 to hostile-authorization-auditor / hostile-exploit-chain-auditor as a
    credential-scope issue: hook dispatch should use a narrower, per-extension or per-hook
    credential (or none at all) rather than the platform-wide SCOS_INTERNAL_SERVICE_TOKEN, and/
    or should restrict hook.Endpoint to an explicit allowlist of hosts rather than "any HTTPS
    host."
  - Ask hostile-integration-auditor or hostile-dependency-supply-chain-auditor to confirm
    Finding #4 against the actual `graphify` CLI's argument-parsing behavior (does it treat a
    leading `-`/`--` value as a flag when positioned as a subcommand's positional argument?);
    if so, graphify-provider should validate/reject such values before invoking the binary.
  - Have someone with runtime access verify the Suspicions section (memory-intelligence's auth
    posture on the graphify-provider ingestion path, and tlsserve's actual TLS/mTLS semantics)
    since both materially change the real-world severity of Findings #1-#3.
```
