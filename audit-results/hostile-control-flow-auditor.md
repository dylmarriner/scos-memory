# Specialist audit artifact

```text
Skill: hostile-control-flow-auditor
Agent: hostile-control-flow-auditor-workflow-agent

Scope:
Trace mcp-gateway, tool-governance, execution-proxy, autonomous-control-plane
request handling from entry to terminal effects (subprocess exec, DB writes,
network calls), focused on services/execution-proxy and
services/tool-governance callers/callees end to end.

Files/components inspected:
- services/mcp-gateway/cmd/server/main.go (lines 1-1065 of 1705; tools/call
  dispatch, dispatchTool, resolveIdentity, governedTools catalogue)
- services/execution-proxy/cmd/server/main.go (full, 1267 lines)
- services/tool-governance/cmd/server/main.go (full, 873 lines)
- services/tool-governance/cmd/server/store.go (full, 314 lines)
- services/autonomous-control-plane/cmd/server/main.go (full, 1276 lines)
- services/autonomous-control-plane/cmd/server/classifier.go (full, 236 lines)
- services/autonomous-control-plane/cmd/server/capabilities.go (full, 340 lines)
- packages/auth/middleware.go (full: AuthenticatedHandler, ControlPlaneHandler,
  claims validation)
- database/policies/000001_rls.sql, 000007_tool_governance_rls.sql (tenant RLS
  on operations / tool_definitions / tool_executions, as cross-check only)
- services/tool-governance/cmd/server/main_test.go (grepped for prepare/policy
  coverage)

Entry points inspected:
- POST /mcp (mcp-gateway, JSON-RPC tools/call) -> dispatchTool() ->
  execution-proxy, tool-governance, autonomous-control-plane endpoints
- POST /v1/control/route, /v1/control/ingest, /v1/triggers (autonomous-control-plane,
  handleIngest -> buildRoute -> executeRoute)
- POST /v1/operations/prepare (tool-governance, handleOperationPrepare) -- the
  capability issuance boundary execution-proxy relies on
- POST /v1/execute, /v1/proxy/execute, /v1/plan, /v1/proxy/explain
  (execution-proxy)

Important callers/callees:
- mcp-gateway.dispatchTool("scos_exec_execute") -> execution-proxy POST /v1/execute
  (services/mcp-gateway/cmd/server/main.go:430-431); args forwarded verbatim as
  the ExecuteRequest body, gateway does not inspect or gate command content.
- execution-proxy.handleExecute -> s.governance.AuthorizeAndPrepare (toolGovernanceClient.post,
  services/execution-proxy/cmd/server/main.go:639-648) -> tool-governance
  POST /v1/operations/prepare -> executeCommand() (/bin/sh -c, main.go:1173-1200,
  called at main.go:688) -- the actual OS-command sink.
- autonomous-control-plane.executeRoute() step "adaptive-execution-proxy" ->
  POST execution-proxy /v1/proxy/execute with confirmed: !decision.ConfirmationRequired
  (main.go:461-466), and step "tool-governance" -> POST tool-governance
  /v1/operations/start (main.go:453-460) -- both unauthenticated by any real
  policy decision beyond the caller's own JWT claims.
- tool-governance.handleOperationPrepare -> registerTool() + transitionTool(...,
  StateAuthorised) whenever the named tool is unknown, then always returns
  {Prepared:true, Authorized:true} (main.go:603-647). No call to security-gate
  (scos_permission_check/classify) or any other policy service occurs anywhere
  in this file or store.go.
- autonomous-control-plane.selectCapabilities()/grantedPermissions() (capabilities.go:246-262)
  grant "tool:invoke"/"execution:invoke" purely from the request's own
  classified intent labels (which are themselves derived from the request's
  own Input text / RiskClass / OperationType fields) -- not from any external
  permission store lookup.

Workflows traced:
1. External MCP client -> mcp-gateway /mcp tools/call scos_exec_execute ->
   execution-proxy /v1/execute -> tool-governance /v1/operations/prepare
   (auto-authorise) -> /bin/sh -c <command> on the execution-proxy host ->
   result persisted (execution-proxy state + raw artifact) -> reconcile ->
   response relayed back through mcp-gateway to the caller. Traced success
   path, dry-run path, and confirmation-required (destructive, unconfirmed)
   rejection path.
2. autonomous-control-plane /v1/control/ingest with proposed_tool = a shell
   command and input = free-form natural language -> buildRoute() ->
   classifyIntent() (scores high_impact_action from Input phrase match or
   explicit risk_class, never from ProposedTool content) -> confirmation
   boolean -> executeRoute() step adaptive-execution-proxy sends
   confirmed: !confirmation to execution-proxy. Traced the case where
   ProposedTool is destructive but Input/RiskClass are innocuous.
3. execution-proxy classifyRisk() keyword scan for an absolute-path command
   (e.g. "/bin/rm -rf /") with no shell metacharacters -- traced through
   validateExecutionRequest and planCommand to confirm it is classified
   "read", bypassing the Confirmed-required and operation_id-required checks
   (though execution still proceeds regardless of classification once
   governance issues an operation).
4. Reconciliation/error path: governance.Reconcile failure after real command
   execution (execution-proxy main.go:697-700) -- command has already run via
   exec.CommandContext before Reconcile is even attempted, so a governance
   reconciliation failure cannot undo or block the completed side effect; it
   only blocks the HTTP response and the proxy's own state persistence.

Trust boundaries:
- External/any-authenticated-principal -> mcp-gateway: auth.AuthenticatedHandler
  (packages/auth/middleware.go:132-166) accepts either the shared static
  SCOS_INTERNAL_SERVICE_TOKEN or any HMAC-valid JWT carrying only tenant_id +
  principal_id claims (no role/scope claim is validated). mcp-gateway is
  documented (docs/agent-connection-guide.md, per task context) as the
  externally-reachable endpoint for Claude Code/Desktop and other MCP clients.
- mcp-gateway -> internal services: adds the shared X-SCOS-Control-Plane-Token
  itself (dispatchTool/copyIdentityHeaders, main.go:780-792) -- so any caller
  who cleared the outer AuthenticatedHandler gate transparently gains the
  control-plane-token-gated surface of execution-proxy and tool-governance;
  the control-plane token is not a second, more privileged credential the
  caller must separately hold.
- execution-proxy/tool-governance -> further trust: auth.ControlPlaneHandler
  requires the control-plane token IN ADDITION to a valid bearer, intended to
  stop a caller with only a normal service credential from bypassing "control-
  plane routing and policy stages" (middleware.go:109-113 comment) -- but as
  traced above, the "policy stage" (tool-governance prepare) performs no
  actual policy evaluation, so this boundary provides authentication-in-depth
  without an accompanying authorization decision.
- autonomous-control-plane RiskClass/Input (client-supplied, unvalidated
  against ProposedTool) is trusted as the sole signal driving confirmation and
  permission grants for the very same request's proposed command.

Attacker-controlled inputs:
- ExecuteRequest.Command (raw shell string executed via /bin/sh -c)
- ExecuteRequest.Confirmed (boolean self-attestation, no independent
  verification)
- ExecuteRequest.OperationID (accepted as tool-governance's operation_id;
  governance does not correlate it against any prior real authorization
  decision -- it is minted or accepted verbatim and always authorised)
- RouteRequest.Input (free text, used only for classifier phrase matching)
- RouteRequest.RiskClass, RequiresFreshData, OperationType (explicit fields
  that override phrase-based classification and directly control
  ConfirmationRequired / grantedPermissions, decoupled from ProposedTool)
- RouteRequest.ProposedTool (the actual command forwarded to execution-proxy,
  never inspected by classifyIntent for high_impact_action)
- tool_name in the tool-governance prepare request (any string auto-registers
  and auto-authorises a new "tool")

Invariants tested:
- "tool-governance is meant to gate what gets executed" (task framing) --
  FALSIFIED: handleOperationPrepare never evaluates ToolProperties.Destructive,
  never checks a per-principal/per-tenant policy, and always returns
  Authorized:true for any never-before-seen tool_name.
- "destructive commands require confirmation" (execution-proxy
  validateExecutionRequest, §20.9 comments) -- holds only against
  execution-proxy's own naive keyword classifier, and Confirmed is a
  caller-supplied boolean with no independent verification; automation
  callers (autonomous-control-plane) can and do set it programmatically from
  signals disconnected from the actual command.
- "classifyRisk conservatively treats a composed command as destructive when
  any destructive token appears" -- holds only when the command contains a
  shell metacharacter; a plain absolute-path invocation (no metacharacters)
  bypasses both the prefix check and the ContainsAny fallback.
- Command execution (exec.CommandContext) is NOT gated by risk classification
  at all -- classification only changes default mode/rewrite/validation
  requirements, never whether executeCommand() runs.

Findings:
1. [Critical] tool-governance's operation/prepare endpoint performs no
   authorization policy decision (services/tool-governance/cmd/server/main.go:543-647).
   Any never-before-registered tool_name is auto-registered with
   Props{ExternallySideEffecting:true, Reconcilable:true}, immediately
   transitioned to StateAuthorised, and the response is always
   {Prepared:true, Authorized:true}. There is no check of the caller's
   permissions, the declared ToolProperties.Destructive/ConfirmationGated
   flags, tenant policy, or any external policy service (security-gate's
   /v1/permissions/check is never called from this file or store.go). Since
   execution-proxy treats a Prepared+Authorized GovernedOperation as the sole
   capability gate before running exec.CommandContext(ctx, "/bin/sh", "-c",
   command) (main.go:639-648, 688), the entire "tool governance" layer is a
   rubber stamp: it produces an audit trail (operations/tool_executions rows)
   but does not restrict what gets executed.

2. [Critical] The end-to-end confirmation/authorization signal for
   command execution is derived from fields disconnected from the actual
   command. autonomous-control-plane's classifyIntent() (classifier.go:107,
   151-155) scores "high_impact_action" (which drives ConfirmationRequired
   and, via selectCapabilities' AllowIrreversible, whether irreversible
   effects are permitted) from either the client-supplied RiskClass field or
   phrase-matching the free-text Input field -- never from RouteRequest.ProposedTool,
   which is the actual string later sent as ExecuteRequest.Command
   (main.go:461-466). A caller (or a compromised/buggy upstream agent) can
   set ProposedTool to a destructive shell command while keeping Input
   innocuous and RiskClass unset/"low", causing
   executeRoute()/adaptive-execution-proxy to send confirmed: true
   automatically to execution-proxy for a command nobody actually confirmed.
   Combined with Finding 1, this command then executes unconditionally.

3. [High] grantedPermissions()/selectCapabilities() (capabilities.go:246-262,
   267-310) self-grant "tool:invoke" and "execution:invoke" purely from the
   request's own classified intent labels (which are themselves computed from
   the request's own fields -- see Finding 2), not from any lookup against a
   real per-principal/per-tenant permission store. There is no call to
   security-gate's scos_permission_check anywhere in
   services/autonomous-control-plane. This means the "RequiredPermissions"
   declared on each CapabilityRecord (e.g. "execution:invoke" on
   adaptive-execution-proxy.execute, capabilities.go:175) are checked only
   against permissions the same request manufactures for itself, not against
   an externally-verified grant.

4. [Medium] execution-proxy's classifyRisk() (main.go:939-999) is a
   prefix/substring keyword scanner that can be trivially evaded for a
   simple (non-composed) command via an alternate invocation path, e.g. an
   absolute path ("/bin/rm -rf /", "/usr/bin/sudo ...") does not match any
   `strings.HasPrefix(lower, kw)` check and, absent a shell metacharacter
   (;|&\n`)$(), never reaches the ContainsAny fallback either -- it is
   classified "read". This downgrades the confirmation requirement and
   operation_id requirement (validateExecutionRequest, main.go:551-560) and
   changes the default safety mode from forensic to adaptive
   (effectiveMode, main.go:85-93), weakening output-reduction/raw-preservation
   guarantees for what is, in fact, a destructive command. Note this does
   NOT by itself allow otherwise-blocked execution (Findings 1-2 mean nothing
   is really blocked), but it does independently defeat the one gate
   (Confirmed-required) that execution-proxy would enforce in isolation if
   Findings 1-2 were fixed.

5. [Low/Hardening] execution-proxy's Reconcile-then-respond ordering means the
   real side effect (exec.CommandContext) has already completed before
   s.governance.Reconcile is called (main.go:688-700); if Reconcile fails the
   handler returns 503 without persisting the result locally, but the command
   has already run on the host and its output is only recoverable via the raw
   artifact directory if persistRaw later succeeds. This is an error-path/
   observability gap more than an authorization gap -- flagged for the
   error-path/persistence auditors to correlate.

Suspicions requiring validation (not independently confirmed in this pass):
- Whether any production deployment configuration (outside this repo's Go
  code -- e.g. an external policy engine, a proxy in front of mcp-gateway, or
  operator-side allowlisting) supplies the missing authorization decision.
  Nothing in services/tool-governance, services/execution-proxy, or
  services/autonomous-control-plane references such a mechanism, and no
  README/GOVERNANCE.md section was located describing where the missing
  check is supposed to live (not read in this pass -- see coverage gaps).
- Whether desktopAgentToolNames narrowing (mcp-gateway main.go:868-875) is
  relied upon as a mitigating control for known desktop-agent integrations;
  it does not apply to unnamed/unknown X-SCOS-Agent-Name callers, which get
  the full governed catalogue including scos_exec_execute.
- Whether ToolProperties.Destructive/ConfirmationGated fields (declared in
  the ToolDefinition schema but never populated or checked by
  handleOperationPrepare) are consumed by some other, unreviewed governance
  code path (e.g. handleOperationStart, which does require
  tool.State == StateAuthorised but is a different endpoint from the one
  execution-proxy actually calls).

Tests/commands executed:
- Read (full file) on all files listed under "Files/components inspected".
- grep -rn "permission" services/{tool-governance,execution-proxy,
  autonomous-control-plane,mcp-gateway} --include=*.go -i (confirmed no
  policy-service call sites outside mcp-gateway's pass-through
  scos_permission_check/classify tools, which nothing in the exec path
  invokes server-side).
- grep -n "prepare|Authorized|rubber|auto-registers|auto-author"
  services/tool-governance/cmd/server/main_test.go (confirmed no test
  exercises or asserts on the auto-authorise rubber-stamp behavior).
- grep -rn "tool_definitions|operations" database/policies/*.sql (confirmed
  RLS scopes these tables by tenant_id only -- orthogonal to the missing
  per-principal authorization decision).
- find services/{execution-proxy,tool-governance,mcp-gateway,
  autonomous-control-plane} -name '*.go' | xargs wc -l (inventory: 7689
  total lines across these four services).
- No Go build/test/run was executed (read-only static trace); this is a
  coverage gap noted below.

Coverage gaps:
- Did not build or run `go test ./services/tool-governance/...`,
  `./services/execution-proxy/...`, `./services/autonomous-control-plane/...`,
  or `./services/mcp-gateway/...` to confirm existing tests pass/fail against
  these findings, or to dynamically confirm the classifyRisk bypass and the
  auto-authorise rubber stamp behave as read statically.
- Did not read services/mcp-gateway/cmd/server/main.go lines 1066-1705 (tool
  schema definitions / remainder of governedTools()); the dispatch logic
  (lines 1-1065) which contains all control-flow-relevant behavior was fully
  read, so this gap is believed low-risk but not verified.
- Did not read services/mcp-gateway/cmd/server/main_test.go or
  services/autonomous-control-plane/cmd/server/{main_test.go,readiness_test.go,
  classifier_test.go} in full -- only grepped tool-governance's test file.
  Existing test coverage/expectations for the findings above are therefore
  not fully characterized.
- Did not inspect services/security-gate (the actual owner of
  /v1/permissions/check and /v1/permissions/classify) to see whether it is
  ever invoked server-side by tool-governance, execution-proxy, or
  autonomous-control-plane through some path not visible via grep (e.g. a
  dynamically constructed URL) -- grep coverage should have caught this, but
  the service's own code was not read to confirm it expects to be called
  from this chain and currently isn't.
- Did not inspect GOVERNANCE.md, docs/agent-connection-guide.md, or other
  design docs referenced in the task framing to check whether the missing
  policy evaluation in tool-governance is an acknowledged known gap /
  roadmap item versus an unintended regression.
- Did not trace the packages/capability-registry.Registry.Select()
  implementation itself (only its call site in capabilities.go) to fully
  verify the AllowIrreversible/ConfirmationGranted enforcement semantics
  inside the registry.
- Did not examine deployment/container configuration (docker-compose,
  k8s manifests, deploy/) for whether SCOS_TOOL_GOVERNANCE_URL is ever
  deliberately left unset in some environment, which would route
  execution-proxy through rejectingGovernance{} (main.go:129-137) and
  fail closed -- worth checking as a possible partial mitigation in some
  deployments.

Areas explicitly not reviewed:
- services/security-gate implementation (auth/permission/audit service) --
  out of this specialist's assigned scope beyond confirming it is not called
  from the traced chain.
- services/agent-registry, services/backup-manager,
  services/compatibility-gateway, services/context-optimisation,
  services/extension-manager, services/memory-gateway,
  services/okf-synchroniser, services/recovery-orchestrator,
  services/retrieval-orchestrator, services/observability,
  services/graphify-provider, services/hermes-provider,
  services/memory-intelligence, services/code-intelligence -- all forwarded
  to by mcp-gateway but outside the assigned scope (execution-proxy /
  tool-governance / mcp-gateway / autonomous-control-plane).
- Frontend/dashboard code, database migrations beyond the RLS policy files
  cited, and deployment/infra configuration.

Recommended follow-up:
- tool-governance: add a real authorization decision to
  handleOperationPrepare -- at minimum, check the declared
  ToolProperties.Destructive/ConfirmationGated flags against the requesting
  principal's actual granted permissions (via security-gate's
  /v1/permissions/check), and refuse to silently auto-authorise a
  previously-unknown, externally-side-effecting tool.
- autonomous-control-plane: compute high_impact_action / ConfirmationRequired
  from the actual ProposedTool/command content (or delegate that
  classification to execution-proxy's own risk classifier, made
  non-bypassable per below) rather than from Input free text and
  client-supplied RiskClass alone; do not let a request author its own
  confirmation signal for its own destructive action.
- autonomous-control-plane: derive grantedPermissions from an authoritative,
  externally-verified permission source per principal/tenant rather than
  self-classification of the same request.
- execution-proxy: replace or harden classifyRisk with a check that
  normalizes/resolves the command's actual invocation path (e.g. via
  basename resolution or a denylist that matches on final path component,
  not just literal string prefix) so `/bin/rm`, `/usr/bin/sudo`, etc. are not
  systematically under-classified.
- Escalate Findings 1-2 to the master auditor / hostile-exploit-chain-auditor:
  they combine into a plausible end-to-end chain from any authenticated MCP
  caller (including, per docs/agent-connection-guide.md context, external
  Claude Code/Desktop clients) through mcp-gateway to arbitrary shell
  execution on the execution-proxy host, gated only by self-attested,
  self-classified fields the same request controls.
```
