# Specialist audit artifact

```text
Skill: hostile-concurrency-auditor
Agent: hostile-concurrency-auditor-workflow-agent

Scope:
  services/execution-proxy (operation_id issuance-then-execution), services/tool-governance
  (approval-then-execution state machine, idempotency store), workers/consolidation (outbox
  job claiming, retry/idempotency of promotion), workers/index-maintenance (retention/vacuum
  cycle), database/policies RLS session-variable handling under pooled connections.

Files/components inspected:
  services/execution-proxy/cmd/server/main.go (full, 1267 lines)
  services/execution-proxy/cmd/server/main_test.go (function inventory only)
  services/tool-governance/cmd/server/main.go (full, 873 lines)
  services/tool-governance/cmd/server/store.go (full, 314 lines)
  services/tool-governance/cmd/server/main_test.go (function inventory only)
  workers/consolidation/src/scos_consolidation/worker.py (full, 511 lines)
  workers/consolidation/src/scos_consolidation/dedup.py (full, 134 lines)
  workers/consolidation/src/scos_consolidation/promoter.py (lines 1-220, id-generation paths)
  workers/consolidation/src/scos_consolidation/extractor.py (id-generation grep)
  workers/consolidation/src/scos_consolidation/review.py (id-generation grep)
  workers/index-maintenance/main.py (full, 155 lines)
  database/policies/000001_rls.sql (policy definitions, current_setting/NULLIF pattern)
  database/migrations/000001_initial.sql (operations, outbox_jobs, idempotency_keys DDL)
  Cross-repo grep of all `set_config(` call sites (11 services + packages/service-state) to
  check for pooled-connection tenant-context leakage.
  services/recovery-orchestrator/cmd/server/main.go (grepped for reconcile/complete callers)

Entry points inspected:
  POST /v1/execute, /v1/proxy/execute, /v1/plan, /v1/proxy/explain (execution-proxy)
  POST /v1/operations/prepare, /v1/operations/start, /v1/operations/complete,
  /v1/operations/reconcile, /v1/tools/register, /v1/tools/transition (tool-governance)
  outbox_jobs polling loop (_claim_jobs/_process_cycle, consolidation worker)
  index-maintenance single-shot maintenance cycle (retention expiry, vacuum)

Callers/callees traced:
  execution-proxy.handleExecute -> toolGovernanceClient.AuthorizeAndPrepare (HTTP POST
    /v1/operations/prepare) -> tool-governance.handleOperationPrepare -> store.startOperation
    (INSERT operations, status='executing') -> back to execution-proxy -> executeCommand
    (exec.CommandContext /bin/sh -c) -> toolGovernanceClient.Reconcile (HTTP POST
    /v1/operations/reconcile) -> tool-governance.handleReconcile -> store.completeOperation
    (UPDATE operations SET status=...).
  consolidation worker._process_cycle -> _claim_jobs (UPDATE ... FOR UPDATE SKIP LOCKED) ->
    _process_job -> extract_candidates -> deduplicate -> detect_conflicts ->
    evaluate_promotion -> _promote_candidate/_write_candidate -> _mark_completed/_mark_failed
    -> conn.commit() once per cycle.

Trust boundaries:
  Caller-supplied ExecuteRequest.OperationID (client/agent controlled) flows unmodified from
  execution-proxy into tool-governance's /v1/operations/prepare as the primary key for the
  `operations` table.
  Idempotency-Key header on the execution-proxy -> tool-governance HTTP call is derived
  deterministically (sha256 of the JSON body) by execution-proxy itself, not the original
  external caller, so distinct external retries of the *same* /v1/execute call collapse to
  the same governance-side idempotency key, but nothing on the execution-proxy side uses that
  fact to suppress re-execution of the shell command.
  Postgres RLS session GUCs (scos.tenant_id/scos.principal_id) set via `set_config(...,true)`
  scoped to the enclosing transaction, consistently across every service that touches
  RLS-protected tables.
  Background workers (consolidation, index-maintenance) connect directly with psycopg using a
  presumably elevated/RLS-bypassing role and rely entirely on explicit `WHERE tenant_id = %s`
  predicates in application SQL, not database RLS, for tenant isolation.

Attacker-controlled inputs:
  ExecuteRequest.Command, ExecuteRequest.OperationID, ExecuteRequest.Mode/RiskTolerance/
  AllowRewrite/Confirmed (execution-proxy); operation_id/tool_name/input on
  /v1/operations/prepare; Idempotency-Key header (governance requires it but the value's
  entropy/reuse is caller-chosen for direct tool-governance callers other than
  execution-proxy); turn/session content that becomes memory_candidates content (indirectly
  drives consolidation dedup fingerprinting).

Invariants tested:
  "An operation_id issued by tool-governance must gate exactly one physical execution of the
  underlying command" (execution-proxy comment: "the proxy never treats a caller-supplied
  operation ID as authorization" -- tested whether it also prevents duplicate *execution*,
  not just duplicate *authorization*).
  "Only an executing operation can be completed/reconciled" (tool-governance's
  StateExecuting -> {StateSucceeded,StateFailed,StateUnknown} transition table).
  "Duplicates SHALL merge evidence rather than produce endless paraphrases" (dedup.py
  docstring, citing spec §16) -- tested whether a dedup match actually prevents a second
  memory_items row from being created.
  outbox_jobs claim exclusivity under concurrent workers (FOR UPDATE SKIP LOCKED correctness).

Findings:

1. [HIGH] execution-proxy re-executes the shell command on every retry of the same
   operation_id, even though tool-governance's own operation-prepare step is idempotent.
   Evidence: services/execution-proxy/cmd/server/main.go:613-756 (handleExecute). After
   `governed, err := s.governance.AuthorizeAndPrepare(...)` returns successfully (line 639),
   execution-proxy unconditionally calls `executeCommand(r.Context(), plan.ExecutedRequest,
   req.WorkingDir)` at line 688 with no prior lookup of `s.results[opID]` (the very map that
   exists at line 248 and is populated at line 741/675) or any other check for "has this
   operation already run". Meanwhile tool-governance's /v1/operations/prepare *is* idempotent
   on the Idempotency-Key (services/tool-governance/cmd/server/main.go:587-601): a retried
   prepare call with the same body returns the cached `Prepared:true, Authorized:true`
   response without re-inserting the operations row. The net effect is that a client/agent
   retry of `/v1/execute` (or `/v1/proxy/execute`) for the same operation_id -- caused by an
   HTTP timeout, a load-balancer retry, or an agent-side retry loop after a transient network
   error, all of which are ordinary and expected in a distributed system -- passes governance
   cleanly a second time and re-runs the underlying `/bin/sh -c <command>` a second time via
   `exec.CommandContext` at line 1182. For destructive commands (`rm`, `git push`,
   `kubectl delete`, `docker rm`, `mv`, etc. per `classifyRisk`, main.go:939-999) this means a
   confirmed, single-intent destructive action can execute twice (or more, on repeated
   retries) with no application-level guard. Confirmed by direct code inspection; no test in
   main_test.go exercises retry/duplicate-operation-id behavior (grep of TestExecute* names,
   none named for retry/duplicate).
   Mitigation search: DryRun path (lines 665-686) *does* short-circuit before executing and
   correctly persists+reconciles without running the command, showing the codebase already
   has the pattern needed (check-then-store) but only applied to the dry-run branch, not the
   real-execution branch guarded by opID.
   Confidence: Confirmed.

2. [HIGH] Duplicate/matched ("merged") memory candidates in the consolidation worker are still
   promoted as brand-new `memory_items` rows, so the outbox job retry path (and, more
   generally, any repeated extraction of the same fact) produces permanent duplicate active
   memories instead of merging evidence, defeating the stated invariant.
   Evidence:
   - workers/consolidation/src/scos_consolidation/extractor.py:98 -- `candidate_id=uuid4()`,
     freshly random on every extraction call, not derived from turn/content/tenant, so the
     *same* underlying turn re-processed (via outbox retry) always yields new candidate ids.
   - workers/consolidation/src/scos_consolidation/dedup.py:21-54 correctly content-fingerprints
     a new candidate against `existing` (memory_items with status='active',
     `_load_existing_memory`, worker.py:288-312) and marks a match `action="merged"`
     (dedup.py:52-53), and its own docstring cites the requirement: "Duplicates SHALL merge
     evidence rather than produce endless paraphrases" (dedup.py:1-4).
   - However, worker.py:194-231 treats `"merged"` results almost identically to `"new"`:
     unless `needs_review`, it calls `_promote_candidate(conn, candidate, promotion)`
     (worker.py:225) regardless of dedup action. `_promote_candidate` -> `build_memory_item`
     (promoter.py:176-198) builds the new `memory_items` row keyed by
     `"id": candidate.candidate_id` -- the *candidate's own* fresh uuid4, never the id of the
     `existing` matched row that `_find_match` returned. `_record_merge` (worker.py:445-467)
     runs afterward, but it only annotates the `memory_candidates` bookkeeping row
     (`conflict_assessment`) with `merged_into`; it performs no UPDATE/skip against
     `memory_items` and does not prevent the `_promote_candidate` INSERT that already ran.
   - Net effect: every dedup "merge" still creates a second, independent, active
     `memory_items`/`memory_versions`/`memory_reviews` row set. Combined with finding #1's
     retry semantics and `_mark_failed` sending jobs back to `status='retryable'`
     (worker.py:496-506) after any exception in `_process_job`, an outbox job that is retried
     for any transient reason (LLM enrichment network hiccup, a later statement's DB error,
     etc.) will re-run `extract_candidates` from scratch and promote a *fresh* duplicate
     memory item on every retry, since `ON CONFLICT (id) DO NOTHING` (used on the
     `memory_items`/`memory_candidates`/`memory_versions` inserts) never fires because the ids
     differ each time.
   - Additional non-idempotent writes on the same retry path: the `memory_evidence`
     (worker.py:411-422) and `memory_reviews` (worker.py:426-436) INSERTs inside
     `_promote_candidate` carry no `ON CONFLICT` clause at all, so even in the narrower case
     where a retry *does* reuse a stable id somewhere, these two tables would still accumulate
     duplicate rows per retry.
   Confidence: Confirmed (by direct code reading of the full promotion path; not exercised by
   a live DB test in this session).

3. [MEDIUM] tool-governance operation state transitions (`operations.complete` and
   `operations.reconcile`) read the current state, decide validity, and then UPDATE without
   re-checking that state in the UPDATE's WHERE clause -- a classic TOCTOU on the state
   machine that allows a stale/duplicate completion or reconciliation call to silently
   overwrite a terminal state.
   Evidence:
   - services/tool-governance/cmd/server/main.go:716-728 (`handleOperationComplete`): loads
     `op` via `loadOperation`, checks `if op.State != StateExecuting { ... 409 ... }`, then at
     line 738 calls `completeOperation(ctx, tx, identity.TenantID, req.OperationID, newState,
     ...)`.
   - services/tool-governance/cmd/server/main.go:810-822 (`handleReconcile`): same pattern --
     checks `op.State != StateExecuting && op.State != StateUnknown && op.State != StateFailed`
     then at line 837 calls the same `completeOperation`.
   - services/tool-governance/cmd/server/store.go:293-313 (`completeOperation`): the UPDATE is
     `UPDATE operations SET status=$1, response=$2, updated_at=now() WHERE id=$3 AND
     tenant_id=$4` -- no `AND status = 'executing'` (or any prior-state) guard. Two concurrent
     calls (e.g. one on `/v1/operations/complete` and one on `/v1/operations/reconcile` for
     the same `operation_id`, or two reconcile calls carrying different Idempotency-Keys
     because their request bodies genuinely differ, e.g. one says `result:"unknown"` and a
     later one says `result:"succeeded"`) can both pass their own state-check read (both see
     `StateExecuting`) before either commits, then both UPDATE the row sequentially once row
     locks are released -- the second UPDATE proceeds unconditionally and silently overwrites
     whatever the first one wrote, with no error surfaced to either caller and no indication
     in the response that a race occurred.
   - This does not require a state re-check across the *same* Idempotency-Key (that path is
     correctly guarded by `idempotentReplay`, store.go:48-67) -- it requires two calls whose
     bodies differ enough to produce different Idempotency-Keys while targeting the same
     `operation_id`, which is plausible whenever an upstream retries with an updated/refined
     result (e.g. execution-proxy's own retry, per finding #1, would call Reconcile twice for
     the genuinely-different `outcome` string of the two separate command executions -- ties
     directly into finding #1's duplicate-execution scenario, where the first execution's
     "succeeded" reconcile could race the second execution's "failed" reconcile, or vice
     versa, and whichever commits last wins with no error).
   Confidence: High-confidence (verified by direct code reading of the exact SQL/Go; requires
   a genuinely concurrent second call with a different body to trigger, which finding #1
   demonstrates is a realistic real-world path).

4. [LOW / hardening] `handleOperationPrepare` accepts an unauthenticated, freeform,
   caller-supplied `operation_id` (services/tool-governance/cmd/server/main.go:624-632,
   sourced from execution-proxy's `ExecuteRequest.OperationID`, itself attacker/agent
   controlled per main.go:54) and uses it directly as the `operations.id` primary key
   (`uuid` typed column, database/migrations/000001_initial.sql:167). A caller that reuses an
   operation_id already used by a *different* tenant/principal cannot succeed (global PK
   collision surfaces as a DB error, `startOperation`/store.go:194-201, mapped to a 500), so
   this is not an authorization bypass, but it is an availability foot-gun: a malicious or
   buggy caller within any tenant can pre-empt (or be pre-empted by) another party's operation
   id with no relation to their own request, turning an otherwise-transient collision into a
   hard failure for the legitimate party ("insert operation" error), and it means operation_id
   collision-based denial-of-service between agents sharing a tenant is possible if operation
   ids are ever derived predictably (e.g. sequential or content-hash based) by any client
   integration.
   Confidence: Plausible (theoretical DoS vector; not verified against a real client that
   generates predictable operation ids).

5. [NON-ISSUE, verified] Postgres RLS session-context handling under pooled connections is
   safe. Every service that sets `scos.tenant_id`/`scos.principal_id` does so with
   `set_config(key, value, true)` (the `is_local=true` / transaction-scoped form) inside an
   explicit `BeginTx`/commit-or-rollback block (grep across
   services/{tool-governance,memory-intelligence,hermes-provider,security-gate,agent-registry,
   autonomous-control-plane,compatibility-gateway,memory-gateway}/**, packages/service-state).
   `is_local=true` GUCs are automatically reset at transaction end regardless of outcome, so a
   pooled connection returned to the pool cannot leak one request's tenant context into the
   next request that reuses the same physical connection. RLS policies themselves fail closed:
   every policy in database/policies/000001_rls.sql compares
   `tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid`, and an unset GUC
   yields SQL NULL, which makes `tenant_id = NULL` evaluate to unknown/false for every row --
   i.e. no-session-context denies all rows rather than permitting them.
   Confidence: Confirmed (no bypass found; documented here so this line of investigation is
   not silently uncovered in the reconciliation).

6. [NON-ISSUE, verified] Consolidation worker's outbox job claiming
   (workers/consolidation/src/scos_consolidation/worker.py:110-139, `_claim_jobs`) uses
   `UPDATE ... WHERE id IN (SELECT id ... FOR UPDATE SKIP LOCKED) RETURNING ...` inside a
   single, not-yet-committed transaction that spans the entire batch's processing
   (`_process_cycle`, worker.py:94-107, commits once at the end). This correctly prevents two
   concurrent worker instances from double-claiming the same row: `SKIP LOCKED` means a second
   worker's claim query simply skips rows still row-locked by the first worker's open
   transaction, independent of the `lease_expires_at` column's wall-clock value, and a crashed
   worker's connection loss releases its locks automatically, reverting the row to its
   pre-claim state with no orphaned "processing" rows.
   Confidence: Confirmed for the claim-exclusivity property specifically. (The same
   single-transaction-per-cycle design is the root cause that makes a DB-level error mid-batch
   poison the *whole* batch's `_mark_failed` bookkeeping -- see Suspicions below -- so this
   property is not cost-free.)

Suspicions requiring validation:
  - workers/consolidation/src/scos_consolidation/worker.py:94-107 (`_process_cycle`): the
    per-job `try/except` in the batch loop catches exceptions and calls `_mark_failed(conn,
    job)` *on the same still-open transaction*. If the exception that triggered the `except`
    branch was itself a Postgres-level error (e.g. a constraint violation from one of the many
    unconditional INSERTs identified in finding #2), the transaction is left in Postgres's
    "aborted" state, and the subsequent `_mark_failed` UPDATE inside the except handler would
    itself raise (`InFailedSqlTransaction`), propagating out of `_process_cycle` uncaught,
    causing the `with psycopg.connect(...)` context manager to roll back the *entire* batch --
    including any already-successful `_mark_completed`/promotion work for other jobs claimed
    in the same cycle. This was not exercised against a live database in this session (no DB
    available) and is flagged as a suspicion rather than a confirmed finding; it would need a
    live-DB or mocked-cursor test that forces a real constraint violation mid-`_process_job` to
    confirm whether `_mark_failed` actually raises in that state and whether the batch-wide
    rollback is observed.
  - Whether any deployed client of execution-proxy (agent runtime, tool-governance itself, or
    an operator script) actually performs HTTP retries against `/v1/execute` with a stable
    `operation_id` was not verified from this repository alone (no such caller is vendored
    here); finding #1 is a confirmed code-level gap regardless, but its real-world exploit
    frequency depends on retry behavior in a caller not present in this repo.
  - Whether `services/tool-governance` ever receives two different-bodied `/v1/operations/
    complete` and `/v1/operations/reconcile` calls for the same `operation_id` in practice
    (beyond the execution-proxy retry scenario in finding #1) was not traced further; other
    potential callers of these endpoints were not enumerated across the whole repo (time
    budget) -- flagged as a coverage gap below.

Tests/commands executed:
  find services/execution-proxy -name '*.go' | xargs wc -l
  find services/tool-governance -name '*.go' | xargs wc -l
  find workers -maxdepth 2 -type d
  find database/policies -type f
  grep -rn "func setPrincipalContext|func startOperation|func completeOperation|
    func idempotentReplay|func recordIdempotent|func loadOperation|func registerTool|
    func transitionTool|func loadTool" services/tool-governance/
  grep -rn "set_config" --include=*.go . (cross-service pooled-connection scan)
  grep -n "operations\\b" database/migrations/*.sql | grep -i "create table"
  sed -n '150,230p' database/migrations/000001_initial.sql
  grep -n "func \\|TestConcurrent\\|Reconcile\\|Complete" services/tool-governance/cmd/server/main_test.go
  grep -n "candidate_id\\s*=\\|uuid4\\|uuid5\\|def build_memory_item\\|def build_memory_version"
    workers/consolidation/src/scos_consolidation/{extractor,promoter,models,review}.py
  grep -n "func Test\\|retry\\|Retry\\|duplicate\\|Duplicate\\|idempot"
    services/execution-proxy/cmd/server/main_test.go
  grep -n "reconcile|Reconcile|operations/complete|StateExecuting|StateUnknown"
    services/recovery-orchestrator/cmd/server/main.go
  No go test / pytest were run (no live database available in this environment; all findings
  are evidence-based on static code/SQL reading, cross-referenced against the actual DDL and
  handler implementations, not executed against a running system).

Coverage gaps:
  - Did not exercise any of the findings against a live Postgres instance (races #1-#3
    reasoned from code/SQL, not reproduced with actual concurrent load).
  - services/tool-governance/cmd/server/main_test.go and
    services/execution-proxy/cmd/server/main_test.go were inventoried by function name only,
    not read line-by-line; it is possible existing tests partially cover adjacent behavior
    that changes the risk calculus (e.g. a test asserting exactly-once persistence at a layer
    not inspected here).
  - workers/embeddings and workers/policy-evaluation were not inspected at all (present in
    workers/ per the initial `find`, out of the scope hint's explicit list, not opened this
    session).
  - Did not enumerate every caller of tool-governance's /v1/operations/complete and
    /v1/operations/reconcile across the whole repo (only execution-proxy and
    recovery-orchestrator were checked); other services may also call these endpoints and
    change the concurrency picture for finding #3.
  - packages/auth/middleware.go and the exact mechanics of X-SCOS-* header trust vs. JWT claim
    trust were taken as background context per the task brief, not independently re-verified
    in this pass.
  - database/policies/000002 through 000009 (governance/root-identity/compatibility/security-
    gate/hierarchy RLS files) were not opened; only 000001_rls.sql was read in detail. The
    tool-governance-specific 000007_tool_governance_rls.sql was not opened despite being
    directly relevant to this service's scope -- flagged as the most actionable gap.
  - No dynamic verification (go test -race, pytest, or a scripted concurrent-request harness)
    was run against execution-proxy or tool-governance in this session.

Areas explicitly not reviewed:
  workers/embeddings/, workers/policy-evaluation/, all database/policies/*.sql except
  000001_rls.sql, services/recovery-orchestrator beyond the single grep above,
  services/autonomous-control-plane's own concurrency properties (only its set_config usage
  was scanned), any frontend/dashboard code, and any Kubernetes/deployment-level retry or
  timeout configuration that would determine how often finding #1's retry scenario actually
  fires in production.

Recommended follow-up:
  1. Fix execution-proxy: before calling `executeCommand`, check whether `s.results[opID]`
     (or a governance-side "already executing/executed" state) already exists for this
     operation_id and, if so, return the cached ExecutionResult instead of re-running the
     command -- mirroring the pattern already used correctly for the DryRun branch. This is
     the highest-priority fix (finding #1).
  2. Fix consolidation worker's promotion path: when `dedup.py` returns `action="merged"`,
     update the *existing* matched `memory_items` row (append evidence, bump version) instead
     of calling `_promote_candidate`, which unconditionally inserts a new row keyed by the
     candidate's own fresh id (finding #2). Also add `ON CONFLICT` handling (or a stable,
     content-derived id) to the `memory_evidence` and `memory_reviews` inserts so a retried job
     cannot duplicate them even in the interim.
   3. Add a state guard to tool-governance's `completeOperation` UPDATE (`AND status =
     'executing'` or the specific prior state expected), and surface a distinguishable
     error/response when the guard fails so a racing caller can detect it was not the writer of
     record (finding #3).
  4. Read database/policies/000007_tool_governance_rls.sql and cross-check it against
     store.go's queries -- this is the single most relevant unread file for this specialist's
     scope and should be the first item in a follow-up pass.
  5. If time allows, add a `go test -race`-driven concurrent-request test hitting
     `/v1/execute` twice with the same operation_id and `/v1/operations/complete` +
     `/v1/operations/reconcile` concurrently for the same operation_id, to convert findings #1
     and #3 from code-reading-confirmed to empirically-reproduced.
```
