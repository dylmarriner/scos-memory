# Specialist audit artifact

```text
Skill: hostile-workflow-state-machine-auditor
Agent: hostile-workflow-state-machine-auditor-workflow-agent
Scope: State machines for services/tool-governance (tool + operation lifecycle),
  services/recovery-orchestrator (recovery run/checklist), services/backup-manager
  (backup/restore/restore-test lifecycle), services/okf-synchroniser
  (proposal lifecycle). Forbidden-transition bypass, stale-state, and
  duplicate-execution bugs.

Files/components inspected:
  - services/tool-governance/cmd/server/main.go (873 lines, full read)
  - services/tool-governance/cmd/server/store.go (314 lines, full read)
  - services/tool-governance/cmd/server/main_test.go (skimmed for canTransition coverage)
  - services/recovery-orchestrator/cmd/server/main.go (564 lines, full read)
  - services/backup-manager/cmd/server/main.go (1217 lines, full read)
  - services/okf-synchroniser/cmd/server/proposals.go (381 lines, full read)
  - services/okf-synchroniser/cmd/server/main.go (not fully read; sync/divergence
    logic only skimmed via proposals.go's calls into s.synchronise/s.documentForPath)
  - database/migrations/000001_initial.sql (operations, idempotency_keys, outbox_jobs tables)
  - database/migrations/000002_governance_tables.sql (tool_executions table)
  - database/migrations/000014_tool_governance_persistence.sql (tool_definitions table)
  - packages/auth/middleware.go (AuthenticatedHandler vs ControlPlaneHandler, lines 108-160)

Entry points inspected:
  - tool-governance: POST /v1/tools/register, /v1/tools/transition,
    /v1/operations/start, /v1/operations/prepare, /v1/operations/complete,
    /v1/operations/reconcile; GET /v1/tools, /v1/tools/{id}, /v1/operations,
    /v1/operations/{id}. Guarded by auth.ControlPlaneHandler (internal
    shared-token only).
  - recovery-orchestrator: POST /v1/recovery/start, /v1/recovery/checklist,
    /v1/recovery/complete; GET /v1/recovery/status[/{id}],
    /v1/recovery/interrupted-turns. Guarded by auth.AuthenticatedHandler
    (accepts internal shared token OR any per-request signed JWT claims --
    no role/scope check).
  - backup-manager: POST /v1/backups/start, /v1/backups/complete,
    /v1/backups/{id}/restore, /v1/restore-tests/start,
    /v1/restore-tests/complete; GET /v1/backups, /v1/restore-tests,
    /v1/restore-tests/cadence, /v1/plan, /v1/retention. Guarded by
    auth.AuthenticatedHandler (same weak per-request-JWT acceptance as above).
  - okf-synchroniser: POST /v1/proposals, /v1/proposals/{id}/{action} where
    action in {validate, diff, review, approve, reject, merge}.

Important callers/callees:
  - tool-governance: canTransition() (main.go:92) is the single declared
    authority for tool_definitions.state transitions but is called from
    exactly one place, handleToolTransition (main.go:382). Direct SQL
    mutators transitionTool() (store.go:167) and completeOperation()
    (store.go:293) are called from handleOperationPrepare (main.go:611,
    auto-authorise path) and handleOperationComplete/handleReconcile
    (main.go:738, 837) without ever consulting canTransition.
  - recovery-orchestrator: handleRecoveryComplete (main.go:243) gates only
    on the in-memory/self-reported RecoveryStatus.Checklist via
    checklistReady() (main.go:346), which is populated either by
    discoverRecoveryFindings() (real DB introspection, main.go:355) or by
    arbitrary caller input through handleChecklistUpdate (main.go:201).
  - okf-synchroniser: proposal() (proposals.go:321) is the sole state guard
    used by validateProposalContext/diffProposal/reviewProposalAuthorized/
    decideProposal/mergeProposal; requireRole() (proposals.go:331) is the
    sole actor check, called from reviewProposalAuthorized (proposals.go:236)
    and decideProposal (proposals.go:260).

Workflows traced:
  1. tool-governance tool lifecycle via explicit endpoint:
     proposed -> prepared -> authorised -> executing -> succeeded/failed/unknown
     -> reconciling -> reconciled, enforced by canTransition() when reached via
     POST /v1/tools/transition.
  2. tool-governance ad hoc/execution-proxy path: POST /v1/operations/prepare
     with an unknown tool_name auto-registers a brand new tool (state
     'proposed') and then calls transitionTool(..., StateAuthorised) directly
     (main.go:611-615), skipping 'prepared' entirely and skipping
     canTransition. The tool is then immediately usable for a live
     'executing' operation in the same request.
  3. tool-governance operation completion: /v1/operations/start or
     /v1/operations/prepare puts an operation in 'executing'
     (store.go:194-217); /v1/operations/complete requires
     op.State == StateExecuting (main.go:725) and writes a terminal state
     via completeOperation(), whose UPDATE has no status precondition in its
     WHERE clause (store.go:298-301, only `id` and `tenant_id`).
  4. tool-governance reconcile: /v1/operations/reconcile accepts
     op.State in {Executing, Unknown, Failed} (main.go:819) -- broader than
     the declared validTransitions table, which only allows Reconciling from
     Failed or Unknown, never from Executing (main.go:83-89).
  5. recovery-orchestrator: /v1/recovery/start seeds a 10-step checklist and
     (if DB configured) runs discoverRecoveryFindings() to populate real
     status. /v1/recovery/checklist lets any caller overwrite any single
     step's Status/Detail to an arbitrary string, with no allow-list
     validation (main.go:201-241, req.Status is inserted into the checklist
     verbatim). /v1/recovery/complete only checks that every step except #10
     is "ok" or "skipped" (checklistReady, main.go:346-353) -- it never
     re-queries discoverRecoveryFindings() or otherwise verifies the
     self-reported checklist against real DB state.
  6. backup-manager local (non-production) backup lifecycle:
     running (handleBackupStart) -> completed (handleBackupComplete, which
     re-verifies the artifact hash from disk, so this path is well-guarded
     for the synthetic/local case). Production path
     (handleProductionBackupStart) completes+verifies atomically in one call
     via pg_dump + AES-GCM + HMAC manifest; verified with
     productionArtifact() (main.go:989) on every subsequent read.
  7. okf-synchroniser proposal lifecycle: proposed -> validated -> diffed ->
     under_review -> approved/rejected -> merged -> indexed, enforced
     stage-by-stage by proposal(id, expectedStatus) (proposals.go:321-330).
     reviewProposalAuthorized (proposals.go:225) requires reviewer !=
     proposer. decideProposal (proposals.go:249), the approve/reject step
     that actually authorises the merge, does not repeat that check: it
     accepts a caller-supplied `req.Reviewer` that only needs a valid
     governance role (requireRole, proposals.go:260), with no comparison
     against p.Proposer.

Trust boundaries:
  - execution-proxy -> tool-governance /v1/operations/prepare: this is the
    boundary the code comments (main.go:537-542) explicitly describe as
    "callers cannot turn an arbitrary operation ID into a capability" --
    but the tool-name-driven auto-authorise path bypasses the tool
    governance review stage for any never-before-seen tool_name.
  - any bearer of a valid per-request JWT (agent-scoped, not necessarily an
    operator/admin identity; see packages/auth/middleware.go:132-160) ->
    recovery-orchestrator and backup-manager, both mounted behind
    auth.AuthenticatedHandler rather than auth.ControlPlaneHandler. This is
    a materially weaker boundary than tool-governance's
    auth.ControlPlaneHandler (internal shared-token only, main.go:187).
  - okf-synchroniser proposal reviewer/approver identity: caller-supplied
    strings (req.Reviewer, req.Role) validated only against a governance
    role table, not against session/authenticated identity -- the service
    trusts the request body for "who is reviewing/approving," not any
    authenticated principal extracted from the request context.

Attacker-controlled inputs:
  - tool-governance: req.ToolName (operation-prepare, arbitrary string,
    used to silently mint+authorise a new tool); req.OperationID
    (operation-prepare, caller-chosen ID inserted directly as the
    operations.id primary key); req.Result / req.NewState (transition and
    complete/reconcile endpoints).
  - recovery-orchestrator: req.Status and req.Detail on
    /v1/recovery/checklist (free-form strings written into the checklist
    with no enum validation); req.ServiceID (arbitrary string used as the
    map/DB key for a recovery run any caller can start, update, or complete).
  - okf-synchroniser: req.Reviewer / req.Role on the approve/reject action
    (proposals.go:117, decideProposal never compares req.Reviewer to
    p.Proposer).

Invariants tested:
  - "A tool must pass through prepared before it is authorised" (declared by
    validTransitions, main.go:80-81) -- VIOLATED by handleOperationPrepare's
    auto-provisioning path.
  - "Operation state transitions must respect the same operation_status
    vocabulary/table used for tools" (asserted by the ToolState doc comment,
    main.go:25-27) -- VIOLATED: handleOperationComplete and handleReconcile
    never call canTransition(), and handleReconcile's ad hoc check permits
    Executing -> Reconciling, which the table forbids.
  - "An operation being completed cannot be completed twice / raced" --
    completeOperation()'s UPDATE has no `AND status='executing'` guard, so
    two concurrent completion attempts that both pass the earlier
    application-level SELECT check can both succeed, last write wins.
  - "Recovery cannot be marked complete while real interrupted turns / open
    operations / pending outbox jobs exist" -- NOT ENFORCED at completion
    time; only enforced indirectly and only if nobody calls
    /v1/recovery/checklist to spoof step status first.
  - "An OKF proposal cannot be approved by its own proposer" -- enforced only
    at the review step (reviewProposalAuthorized), not at the terminal
    approve/reject step (decideProposal).

Findings:
  See structured findings in the tool response. Summary below with evidence.

  [F1] HIGH -- tool-governance: forbidden transition proposed->authorised
       bypasses canTransition() and the 'prepared' gate.
       Evidence: services/tool-governance/cmd/server/main.go:603-622 (the
       `case err == sql.ErrNoRows` branch inside handleOperationPrepare)
       calls registerTool() (leaves tool in StateProposed) then immediately
       transitionTool(ctx, tx, identity.TenantID, tool.ID, StateAuthorised)
       (main.go:611) with no call to canTransition(). validTransitions
       (main.go:79-90) only allows StateProposed -> {StatePrepared,
       StateCancelled}; StateProposed -> StateAuthorised is not in the
       table and is rejected by handleToolTransition (main.go:382) if
       attempted through the "front door." The auto-provision path reaches
       the same outcome through a side door that skips the check entirely,
       then starts a live 'executing' operation in the same request
       (main.go:633, startOperation). Any principal able to call
       /v1/operations/prepare with a novel req.ToolName (documented as the
       execution-proxy boundary) can mint and immediately execute an
       "authorised" tool that never went through the 'prepared' review
       stage the state machine otherwise requires.

  [F2] MEDIUM -- tool-governance: operation completion race / no
       compare-and-swap on terminal transition.
       Evidence: services/tool-governance/cmd/server/store.go:293-301,
       completeOperation()'s UPDATE statement:
       `UPDATE operations SET status=$1, response=$2, updated_at=now()
        WHERE id=$3 AND tenant_id=$4` -- no `AND status='executing'`
       predicate. Both handleOperationComplete (main.go:716-725) and
       handleReconcile (main.go:810-819) load the operation with a plain
       SELECT (loadOperation, store.go:219-247, no `FOR UPDATE`), check
       op.State in application code, and only then call completeOperation.
       Two concurrent requests (e.g. a legitimate /v1/operations/complete
       from execution-proxy racing an /v1/operations/reconcile call) can
       both pass their state check against the same pre-image and both
       issue their UPDATE; whichever commits last silently overwrites the
       other's terminal state and output/error payload with no error
       surfaced to the loser (whose transaction still commits
       successfully). This also duplicates the idempotency-key bookkeeping
       under two different keys for what the system now believes is one
       terminal transition.

  [F3] MEDIUM -- tool-governance: handleReconcile permits Executing ->
       Reconciling, contradicting the declared validTransitions table.
       Evidence: services/tool-governance/cmd/server/main.go:819
       `if op.State != StateExecuting && op.State != StateUnknown &&
        op.State != StateFailed { ... 409 ... }` accepts StateExecuting,
       but validTransitions (main.go:83, 85, 87) only allows
       StateReconciling to be reached from StateFailed or StateUnknown, not
       StateExecuting. An operation still actively executing can be forced
       into 'reconciling' (or, via req.Result, straight to 'succeeded'/
       'failed') through this alternate endpoint without ever going through
       StateUnknown/StateFailed, which is also the mechanism the
       recovery-orchestrator uses to find "open operations needing
       reconciliation" (its query is `status IN ('executing','unknown',
       'reconciling')`, main.go:374) -- so this path can make an operation
       that is genuinely still running disappear from that discovery query
       prematurely.

  [F4] HIGH -- recovery-orchestrator: recovery completion is gated on a
       fully caller-writable, unvalidated checklist.
       Evidence: services/recovery-orchestrator/cmd/server/main.go:201-241
       (handleChecklistUpdate) copies req.Status and req.Detail verbatim
       into status.Checklist[i] with no validation against the comment's
       own documented enum (main.go:39, "pending, verifying, ok, failed,
       skipped"). handleRecoveryComplete (main.go:243-284) then gates solely
       on checklistReady() (main.go:346-353), which only inspects the
       in-memory Checklist slice -- it never re-runs discoverRecoveryFindings
       or otherwise re-verifies against interrupted turns / open operations
       / pending outbox jobs. Any caller who can reach this
       auth.AuthenticatedHandler-protected endpoint (any valid internal
       token OR any per-request signed JWT, packages/auth/middleware.go:
       132-160 -- no role/scope restriction) can POST
       /v1/recovery/checklist with status:"ok" for every step 1-9, then
       POST /v1/recovery/complete, forcing Phase="completed" and recording
       a "recovery_completed" event/lease-release (main.go:497-509) while
       real interrupted turns and open operations remain unresolved. This is
       a stale-state/self-attestation bypass of the entire recovery
       invariant the checklist exists to enforce.

  [F5] LOW/MEDIUM -- recovery-orchestrator: no per-tenant or per-service
       authorization on recovery endpoints; req.ServiceID is an arbitrary
       caller-chosen key.
       Evidence: services/recovery-orchestrator/cmd/server/main.go:148-199,
       201-241, 243-284 -- none of handleRecoveryStart/handleChecklistUpdate/
       handleRecoveryComplete check the caller's identity against
       req.ServiceID (there is no resolveIdentity-equivalent at all in this
       service). Any authenticated caller can start, update, and complete
       the recovery run for any service_id, including one it does not own
       ("default" or another service's name), which combined with F4 lets
       an attacker who only holds an ordinary per-request JWT camouflage or
       falsely resolve an ongoing incident for a service they have no
       operational relationship to.

  [F6] HIGH -- okf-synchroniser: proposal self-approval possible at the
       decision step even though the review step forbids it.
       Evidence: services/okf-synchroniser/cmd/server/proposals.go:225-248
       (reviewProposalAuthorized) enforces
       `if strings.TrimSpace(reviewer) == "" || reviewer == p.Proposer {
        return nil, errors.New("independent authorised review is required") }`
       at the 'review' step. decideProposal (proposals.go:249-272), which
       performs the actual approve/reject transition that gates
       mergeProposal, has no equivalent check: `reviewer` defaults to
       `p.Reviewer` only when req.Reviewer is empty (proposals.go:254-256),
       otherwise it uses whatever req.Reviewer the caller supplied,
       validated only by requireRole() (proposals.go:260, which checks role
       assignment, not identity-vs-proposer). A proposer who also holds a
       governance role (e.g. knowledge_curator) can move their own proposal
       from 'under_review' to 'approved' by calling the "approve" action
       with reviewer set to themself, defeating the separation-of-duties
       invariant the review step establishes and enabling a single
       principal to author and approve an OKF authoritative-document change
       that mergeProposal (proposals.go:273) will then commit to the
       protected branch.

  [F7] LOW -- backup-manager: local-mode backup IDs derived from
       time.Now().UnixNano() are not guaranteed unique under concurrent
       /v1/backups/start calls.
       Evidence: services/backup-manager/cmd/server/main.go:746
       `func (s *backupServer) nextID() string { return fmt.Sprintf(
        "backup-%d", time.Now().UnixNano()) }` -- two concurrent start
       requests landing in the same wall-clock nanosecond (platform/VM
       clock-resolution dependent) would collide on s.artifactPath(id) and
       overwrite each other's artifact file; this only affects the
       synthetic local/test provider, not the production pg_dump path,
       which shares the same nextID() helper (main.go:879) but is
       otherwise protected by full content/HMAC verification on read.

Suspicions requiring validation:
  - Whether mcp-gateway (the externally-reachable MCP endpoint) or any other
    externally reachable service actually forwards ordinary agent-issued
    JWTs to recovery-orchestrator or backup-manager in this deployment; if
    it does not, F4/F5's practical exposure is limited to other internal
    services holding a JWT/shared token, which is still a materially larger
    trust set than "operators only." I did not trace the full network
    topology / service-mesh policy to confirm or rule this out.
  - Whether okf-synchroniser's HTTP handlers (main.go, not proposals.go)
    resolve `req.Reviewer` from an authenticated principal anywhere upstream
    of proposals.go, which would mitigate F6; my reading of proposals.go
    shows no such resolution, but I did not fully read main.go's routing/
    auth wiring for this service.
  - Whether execution-proxy is the only realistic caller of
    /v1/operations/prepare with attacker-influenced tool_name values, or
    whether an agent's tool-call payload can reach req.ToolName more
    directly; this affects how directly attacker-controlled F1's
    tool_name is.

Tests/commands executed:
  - grep -n "canTransition" services/tool-governance/cmd/server/*.go
    (confirmed canTransition is referenced only in main_test.go and once in
    main.go:382; never called from store.go's transitionTool/
    completeOperation call sites).
  - grep -n "CREATE TABLE.*operations|CREATE TABLE.*tool_definitions|
    CREATE TABLE.*tool_executions|CREATE TABLE.*idempotency_keys"
    database/migrations/*.sql (confirmed operations.id is a global uuid
    PRIMARY KEY with no tenant scoping in the key itself, and
    tool_definitions has UNIQUE(tenant_id, name)).
  - grep -n "func AuthenticatedHandler|func ControlPlaneHandler"
    packages/auth/middleware.go (confirmed ControlPlaneHandler requires a
    static X-SCOS-Control-Plane-Token in addition to AuthenticatedHandler's
    checks, while AuthenticatedHandler alone accepts any validly-signed
    per-request JWT with config.IssuerSecret set, with no role/scope
    checked at the middleware layer).
  - grep -rn "recovery/checklist|recovery/complete|recovery/start" (excluding
    the service's own main.go and _test.go) across the repo: only
    indexes/CODE_INDEX.md references these routes; no other in-repo service
    was found calling them, suggesting these are intended for an external/
    operator tool not present in this repository, which does not change
    the authorization gap at the HTTP boundary itself.
  - Did not run `go build`/`go test` for these services in this pass
    (no go toolchain invocation attempted); all findings are from static
    reading of the exact source lines cited above, not from executing the
    code.

Coverage gaps:
  - services/okf-synchroniser/cmd/server/main.go (routing, s.synchronise,
    s.documentForPath, divergence/projection state) was not read in full --
    only its call sites reached from proposals.go were inspected. The
    synchronise/projection state machine (okf_document/okf_projection
    status fields referenced at proposals.go:293-297) was not independently
    reconstructed.
  - services/tool-governance/cmd/server/main_test.go and the other
    services' *_test.go files were not read for their full assertions,
    only grepped for canTransition usage; I have not confirmed whether
    existing tests already cover/expect any of F1-F3.
  - Did not trace how execution-proxy actually calls
    /v1/operations/prepare (services/execution-proxy was out of scope for
    this pass) to confirm the practical attacker-reachability of
    req.ToolName/req.OperationID end-to-end from an agent's tool call.
  - Did not examine database/policies/*.sql RLS policies for
    recovery_runs/recovery_events/recovery_leases/okf_proposals/
    okf_proposal_transitions to see whether row-level security independently
    restricts which principal can write these rows beyond what the Go code
    enforces (recovery-orchestrator's persistRecoveryStatus only sets
    `scos.service_id`, never `scos.tenant_id`/`scos.principal_id`, which is
    itself notable but not independently verified against the RLS policy
    definitions).
  - backup-manager's encryption/HMAC key handling (backupKey(),
    manifestMAC()) and restoreProduction()'s shell-out to
    createdb/pg_restore were read but not adversarially tested for command
    injection via id/name construction; `name` is built from a
    caller-uncontrolled server-generated id plus UnixNano, so injection risk
    there looks low, but this was not exhaustively verified against every
    call site.
  - Did not attempt to reproduce F2's race with an actual concurrent test
    run; the finding rests on static analysis of the missing WHERE-clause
    guard and the absence of `SELECT ... FOR UPDATE`.

Areas explicitly not reviewed:
  - services/execution-proxy (out of scope per the task's scope hint, though
    it is the primary caller referenced by tool-governance's own comments).
  - mcp-gateway routing/proxying rules that determine which internal
    services an external client's JWT can ultimately reach.
  - database/policies/*.sql RLS policy bodies (only table DDL was read).
  - Any non-Go/deployment configuration (Kubernetes/Docker network policy)
    that might restrict which callers can physically reach
    recovery-orchestrator/backup-manager's ports regardless of the
    application-layer auth gap noted in F4/F5.

Recommended follow-up:
  - tool-governance: route handleOperationPrepare's tool auto-provisioning
    through the same canTransition()-checked path as handleToolTransition,
    or explicitly document and test the proposed->authorised shortcut as an
    intentional, narrowly-scoped exception (and consider requiring the
    'prepared' step even for auto-provisioned tools before they can execute).
  - tool-governance: add `AND status = $expected` to completeOperation's
    UPDATE (or use `SELECT ... FOR UPDATE` inside the transaction) so a
    losing concurrent writer gets a detectable 0-rows-affected instead of
    silently overwriting a terminal state; make handleReconcile call
    canTransition() (or an operation-specific equivalent) instead of its
    ad hoc state list.
  - recovery-orchestrator: validate req.Status against the documented enum
    in handleChecklistUpdate; have handleRecoveryComplete re-run (or at
    least spot-check) discoverRecoveryFindings() before flipping Phase to
    "completed" rather than trusting the in-memory checklist alone; add an
    identity/role check (or move this service behind ControlPlaneHandler)
    so arbitrary per-request JWTs cannot start/checklist/complete recovery
    for services they do not operate.
  - okf-synchroniser: add the same `reviewer != p.Proposer` check inside
    decideProposal that reviewProposalAuthorized already enforces, and
    consider binding req.Reviewer to the request's authenticated principal
    rather than trusting the request body.
  - backup-manager: make nextID() collision-resistant (e.g. append a random
    suffix or use the newID()-style UUID pattern already used by
    tool-governance) rather than relying solely on nanosecond timestamp
    uniqueness.
```
