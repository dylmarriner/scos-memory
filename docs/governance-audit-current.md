# Governance Audit — Current (Consolidated)

**Authority audited:** `GOVERNANCE.md` v1.3.0 (2766 lines, sections 1-43)
**Audit date:** 2026-07-31
**Scope:** current working tree, all 43 sections
**Supersedes:** `docs/governance-implementation-audit.md` and
`docs/governance-audit-full.md`, both deleted by this document's commit.

## Why the two prior docs are superseded, not just merged

Both prior documents were internally honest about their own limits, but both
had gone stale against a working tree that kept moving after they were
written, and both explicitly declined to exhaustively re-derive roughly a
third of the document (sections 8-12, 14-16, the general mechanics of 19-23,
28-35, 38, 40, 43 — see `governance-audit-full.md` lines 89-95, and the
"Post-Audit"/"Remediation Tranche" narrative style of both files that mixes
findings from several different points in time in one running document).
Two concrete, checkable examples of staleness that this pass verified before
writing this document:

1. **Messages append-only guard.** `governance-audit-full.md` (P0 finding 1,
   lines 103-115) states `messages` is "mutated in place... unprotected by
   any append-only guard," citing `journal.go:559`. That finding predates
   `database/migrations/000010_messages_append_only.sql`, which is present in
   the current tree and installs a real trigger
   (`messages_finalized_append_only` / `reject_finalized_message_mutation`)
   that raises an exception on any UPDATE to a finalized message row. The
   older `governance-implementation-audit.md` already recorded this fix in
   its own "Remediation Tranche" section — the two documents contradicted
   each other on the current state of the same table, which is precisely
   the ambiguity this consolidation removes.
2. **Benchmark corpus size.** `governance-audit-full.md`'s executive summary
   and P0 finding 3 state the superiority gates rest on "only 3-4 synthetic
   hand-written cases per domain." The current `benchmarks/acceptance-results.json`
   contains 24 context-optimisation cases, 19 code-intelligence cases, 20
   execution-proxy cases, and 24 control-plane-routing cases (verified this
   pass by re-running `benchmarks/run_acceptance.py` and diffing the output
   byte-for-byte against the committed file — reproducible, not stale). The
   3-4 figure was accurate on the day it was written and has since been
   superseded by two later remediation tranches recorded in
   `governance-implementation-audit.md` itself ("Benchmark corpora expanded" /
   "grown further" sections) — again, the fix and the still-citing FAIL
   verdict were live in the repo at the same time, in two different files.

Rather than patch two documents that already disagree with each other and
with the tree, this single document is the current source of truth. It does
not repeat the two files' full findings verbatim; it restates each surviving
verdict against fresh evidence (this pass, for sections 8-12, 14-16, 19-23,
28-35, 38, 40, 43) or explicitly marks older sections as carried forward
un-re-audited (sections 1-7, 17-18, 24-27, 36-37, 39, 41-42), consistent with
this pass's assigned scope. Carried-forward sections are **not** re-derived
here — where the prior docs recorded a FAIL/PARTIAL for them, that verdict
stands until a future pass re-verifies it against code.

## Master Section Table (all 43 sections)

Legend: **Fresh** = independently re-verified against code in this pass.
**Carried** = not in this pass's scope; verdict taken from the more recent
of the two prior audits (`governance-audit-full.md`, 2026-07-27) and not
independently re-checked here.

| § | Topic | Status | Basis |
|---|---|---|---|
| 1 | Executive ruling | FAIL (Carried) | Full lifecycle enforcement across all services not independently re-derived this pass; carried from prior audit. |
| 2 | Normative language / compliance gate | PARTIAL (Carried) | `scripts/audit_governance.py` runs and passes structurally this pass (`GOVERNANCE AUDIT: PASS`), consistent with prior "restored" state; not otherwise re-derived. |
| 3 | Objectives / RPO-RTO | FAIL (Carried) | Not re-verified this pass. |
| 4 | Governing principles (append-only correction) | PASS (Carried, confirmed) | `messages_finalized_append_only` trigger (migration 000010) confirmed present in this pass's repo read — closes the specific §4.6 gap both prior docs discussed; broader §4 scope not otherwise re-derived. |
| 5 | Standards baseline | PARTIAL (Carried) | TLS/OTEL items remain deployment-owned per prior audits; not re-derived. |
| 6 | Logical architecture | FAIL (Carried) | Not re-verified this pass. |
| 7 | Component responsibilities | FAIL (Carried) | Not re-verified this pass. |
| 8 | Memory taxonomy | PASS (Fresh) | `core-durability` group; no gap found. |
| 9 | Memory scopes | PASS (Fresh) | `core-durability` group; no gap found. |
| 10 | Authority hierarchy | PASS (Fresh) | `core-durability` group; no gap found. |
| 11 | Turn durability protocol | PASS (Fresh) | `core-durability` group; message/operation/chunk/turn-completion atomicity confirmed. |
| 12 | Event model | **PASS (Fresh, fixed this pass)** | Correlation ID / causation ID were never populated on any event row; fixed in `services/memory-gateway/cmd/server/journal.go` (`appendEvent`). See Fixes below. |
| 13 | Persistence model | PASS (Carried) | Table coverage confirmed by prior audit; one accepted naming drift (`outbox_dead_letters` vs. spec's `dead_letter_jobs`), documented via ADR-0003. |
| 14 | Transactional outbox | PASS (Fresh) | `core-durability` group; same-transaction write, `FOR UPDATE SKIP LOCKED` claim, idempotent replay, dead-lettering all confirmed. |
| 15 | Idempotency | PASS (Fresh) | `core-durability` group; all mutating gateway handlers require and enforce `Idempotency-Key`. |
| 16 | Memory consolidation policy | PASS (Fresh) | `core-durability` group; promotion gate, human-review classes, dedup/merge all confirmed. |
| 17 | Retrieval | PARTIAL (Carried) | Namespace-membership check confirmed present by prior audit; not re-derived this pass. |
| 18 | Context optimisation | PARTIAL (Carried) | Not re-verified this pass. |
| 19 | Code intelligence | **PASS (Fresh, fixed this pass)** | `native-planes` group fixed cross-tenant leakage (19.10) and missing impact-reconciliation learning signal (19.9). See Fixes below. |
| 20 | Execution proxy | **PASS (Fresh, fixed this pass)** | `native-planes` group added the five required safety modes (20.9), previously entirely absent. See Fixes below. |
| 21 | Autonomous control plane | PASS (Fresh) | `native-planes` group; mandatory-ingress, lifecycle persistence, and RLS parity confirmed, including the specific `orchestration_runs` RLS question raised by a prior audit (resolved: RLS lives in `database/policies/000002_control_plane_rls.sql`, applied via the policies directory rather than the migration that created the table). |
| 22 | Extension platform | **PARTIAL (Fresh, partially fixed this pass)** | `extension-compat` group fixed missing rollback-plan fail-closed gate (22.2) and non-canonical isolation-mode strings (22.4). Two genuine gaps remain open — see Open Items. |
| 23 | Compatibility architecture | **PASS (Fresh, fixed this pass)** | `extension-compat` group fixed incomplete integration-boundary table (23.2), unclassified adapter operations (23.4), envelope-section data loss (23.5), and missing experimental-boundary labelling (23.7). See Fixes below. |
| 24 | Hermes provider | PARTIAL (Carried) | Not re-verified this pass; prior audit found most lifecycle methods implemented, a few (`queue_prefetch`, `get_config_schema`, etc.) not confirmed as distinct handlers. |
| 25 | Memory intelligence / dashboard | PARTIAL (Carried) | Prior remediation tranches added 18/19 views plus a live-trace panel; stream still carries only connected/heartbeat frames per prior audit. Not re-verified this pass. |
| 26 | OKF governance | PARTIAL (Carried) | PostgreSQL-backed projections confirmed added (migration 000011) by prior audit; Git commit tracking / protected-branch approval / import-export divergence handling still open per that audit. Not re-verified this pass. |
| 27 | Security model | PARTIAL (Carried) | Compose-level self-signed TLS added per prior audit; production PKI/cert rotation explicitly deployment-owned. Not re-verified this pass. |
| 28 | Tool governance | PASS (Fresh) | `ops-recovery` group; state machine, `ToolProperties`, idempotency all confirmed matching GOVERNANCE.md verbatim. |
| 29 | Multi-agent coordination | **PASS (Fresh, fixed this pass)** | `ops-recovery` group fixed incomplete agent-registration field validation; handoff-packet structure confirmed already complete. See Fixes below. |
| 30 | Crash recovery | **PASS (Fresh, fixed this pass)** | `ops-recovery` group fixed three checklist steps (OKF state, checkpoint load, abandoned-lease reset) that were unconditionally marked "skipped" despite the backing schema existing. See Fixes below. |
| 31 | Backup and disaster recovery | **PASS (Fresh, fixed this pass, code-owned parts)** | `ops-recovery` group fixed the never-wired `GitMirrors` verification gate; base backups/encryption/retention/restore-drill already confirmed real. Real off-host mirror execution remains deployment-owned. See Fixes/Open Items. |
| 32 | Observability | **PARTIAL (Fresh, partially fixed this pass)** | `observability-governance` group fixed missing task/operation correlation IDs on all three signal types. A genuine cross-service instrumentation-rollout gap remains open — see Open Items. |
| 33 | Quality evaluation / benchmark thresholds | PASS (Fresh) | `observability-governance` group; confirmed thresholds match GOVERNANCE.md exactly and the 24/19/20/24-case corpus reproduces byte-identically. |
| 34 | Governance roles | PASS (Fresh) | `observability-governance` group; all 8 roles bound in `okf/governance/roles.md` with reassignment procedure and separation-of-duty note. |
| 35 | Change governance | PASS (Fresh) | `observability-governance` group; ADRs 0001-0010 confirmed to cover the required decision surface. |
| 36 | Implementation baseline | PARTIAL (Carried) | Docker hardening (pinned images, non-root) confirmed by prior audit; production secret/key management deployment-owned. Not re-verified this pass. |
| 37 | Repository layout | PARTIAL (Carried) | Naming drift (`execution-proxy`, dashboard/transparency-projection split) already documented in ADR-0003 per prior audit. Not re-verified this pass. |
| 38 | Required interfaces | PASS (Fresh) | `contracts-crosscheck` group; every OpenAPI path cross-checked against a real, authenticated handler registration across all 19 services. |
| 39 | Failure-injection tests | PASS (Carried, not re-litigated per task scope) | 58/58 scenarios previously run live against a real 19-service compose stack across three consecutive runs, per `governance-implementation-audit.md`'s "Live Failure-Injection Verification." Not re-run this pass. |
| 40 | Delivery phases | PASS (Fresh) | `contracts-crosscheck` group; every named phase maps to an existing, buildable service. |
| 41 | Definition of done | PARTIAL (Carried, synthesized) | Cannot be marked PASS while §22 canary installs, §32 cross-service metric rollout, full §25 integration evidence, and several Carried-FAIL/PARTIAL sections above remain open. |
| 42 | Prohibitions | PASS (Fresh follow-up, see addendum) | Two further fabricated-state instances were found and fixed in a follow-up pass after this document was first written: `compatibility-gateway`'s adapter `HealthStatus` was a hardcoded `"healthy"` constant never updated by any real check, and `memory-intelligence`'s `/v1/dashboard/system` System Map (§25.9) was a fully static stub (`Status: "unknown"`/`Uptime: "unavailable"` for all 8 entries, unconditionally, regardless of real service state). Both are now backed by real measurement — see addendum below. |
| 43 | Final architecture decision | PASS (Fresh) | `contracts-crosscheck` group; every named architectural component maps to a concrete, present, compiling service. |

## What Got Fixed In This Pass (with evidence)

- **§12 Event model — correlation/causation IDs.** `services/memory-gateway/cmd/server/journal.go`, `appendEvent` (previously ~line 617): the CloudEvents envelope and the `events` INSERT never populated `correlation_id`/`causation_id`, though both columns exist in `database/migrations/000001_initial.sql:142-143`. Fixed by looking up the immediately-preceding event for the same `aggregate_id` as `causation_id` and threading `correlation_id` = `aggregate_id` across all events for a turn/session; `appendEvent` now returns the inserted event's ID.
- **§19.10 Code intelligence — cross-tenant leakage.** `services/code-intelligence/cmd/server/main.go`: `handleRepos`/`handleRepoByID`/query/context/impact endpoints had no tenant scoping at all (repo-wide grep for "tenant" returned zero hits pre-fix). Fixed by adding `TenantID` to `Repository`/`CodeQuery`, a `requireTenant()` gate reading the `X-SCOS-Tenant-ID` header set by the control plane, and filtering `scopedSymbols` by owning-repository tenant.
- **§19.9 Code intelligence — impact reconciliation.** Same file: `handleImpact` computed predicted risk but nothing reconciled it against observed outcomes. Fixed by adding a persisted `ImpactReconciliation` record and a new `POST /v1/impact/reconcile` handler computing `dependant_count_error` / `outcome_mismatch` as the durable learning signal.
- **§20.9 Execution proxy — safety modes.** `services/execution-proxy/cmd/server/main.go`: zero support existed for the five required modes (bypass/observe/safe/adaptive/forensic) pre-fix (grep for "mode" returned zero hits). Fixed by adding `Mode` to `ExecuteRequest`/`CommandPlan`/`ExecutionResult` and `effectiveMode()`/`reducesOutput()`/`rewriteEligible()` helpers implementing the governance table; destructive-risk commands now default to forensic.
- **§22.2 Extension platform — fail-closed rollback plan.** `services/extension-manager/cmd/server/main.go`: `ExtensionManifest` had no `rollback_plan` field, so installation could never fail closed on its absence. Fixed by adding `RollbackPlan` (required)/`MigrationPlan` (optional) and wiring the existing `validateInstallManifest()` hard-rejection path to it.
- **§22.4 Extension platform — canonical isolation strings.** Same file: `determineIsolation()` returned non-canonical strings (`wasm`, `in_process`) instead of the governance-defined set; fixed to return `in_process_trusted`/`wasm_sandbox`/`remote_endpoint` etc.
- **§23.2 Compatibility — integration boundary coverage.** `services/compatibility-gateway/cmd/server/main.go`: only 5 of the 10 required boundaries were registered pre-fix. Fixed by adding `model_api_proxy`, `provider_plugin`, `lifecycle_hooks`, `event_bridge`, `a2a_adapter`.
- **§23.4 Compatibility — adapter operation classification.** Same file: several of the 18 required contract operations were left unclassified for every non-`native_sdk` adapter (neither Supported nor Unsupported). Fixed by fully classifying all 18 operations for every adapter and adding a regression test guarding future completeness.
- **§23.5 Compatibility — envelope rendering data loss.** Same file: `renderMarkdown`/`renderText` silently dropped 4 of 11 envelope sections (episodic history, code references, artifacts, restoration references). Fixed by rendering all sections in both projections.
- **§23.7 Compatibility — untested boundary labelling.** Same file: `Adapter` had no maturity indicator. Fixed by adding `Tested bool`, marking the 5 newly-added boundaries `false`.
- **§29 Multi-agent coordination — registration field validation.** `services/agent-registry/cmd/server/main.go`: `handleAgentRegister` only checked `Name != ""`, silently accepting agents missing version/maintainer/model-provider/cost-policy/health-endpoint. Fixed by adding `validateAgentRegistration()` requiring all six.
- **§30 Crash recovery — falsely-skipped checklist steps.** `services/recovery-orchestrator/cmd/server/main.go`: steps 3 (OKF repository state), 4 (checkpoint load), and 8 (abandoned-lease reset) were unconditionally marked `"skipped: not represented by the recovery database schema"` even though `okf_bundles`, `checkpoints`, and `recovery_leases` tables already exist and were simply never queried. Fixed by querying/resetting against those real tables.
- **§31 Backup and disaster recovery — unwired Git-mirror gate.** `services/backup-manager/cmd/server/main.go`: `BackupPlan.GitMirrors` existed in the struct/API shape but was never set. Fixed by wiring it to new `SCOS_OKF_GIT_MIRROR_REMOTE`/`SCOS_OKF_GIT_MIRROR_VERIFIED` env gates, following the same pattern as the other verified-backup flags, and requiring it in `productionConfigError()`.
- **§32 Observability — missing task/operation correlation.** `services/observability/cmd/server/main.go` and `otlp.go`: `MetricPoint`/`TraceSpan`/`LogEntry` carried trace/span/session/turn/agent/tenant IDs but not task or operation IDs, despite `operation_id` being an established first-class concept elsewhere in the codebase. Fixed by adding both fields to all three types and threading them through OTLP export.

Each fix above shipped with new or updated tests in its owning service's
`main_test.go`, and all six audit groups' `go build`/`go test` runs (and the
Python suites they touched) passed after the fixes, per each group's own
`build_test_status` and the final combined verification below.

## What Remains Open

### Genuinely deployment-owned (not code gaps — correctly left unfixed)

- Real TLS certificates / production PKI and certificate rotation for
  service-to-service traffic (compose currently uses a self-signed dev CA;
  §27, carried).
- Off-host object storage and the actual execution of Git-mirror /
  encrypted-off-host backup pushes against a real remote target — the
  code-level verification *gates* for these are now wired (§31, this pass),
  but performing a real push to a real remote host is infrastructure the
  test environment does not have.
- A live OTEL/OTLP collector endpoint to receive the exporter's traffic —
  the exporter code and correlation fields are complete and tested (§32,
  this pass), but no live collector runs in this environment.
- Production secret/key management (real KMS-backed secrets rather than
  `${VAR:?required}` env placeholders; §5, §36, carried).

### Genuinely still-open code work

- **§22.3 — no true side-by-side canary versions.** Extension
  install/rollback supports one linear undo stack per package, not
  concurrently-active versions for canary testing. Real gap, not attempted
  this pass.
- **§22.6/22.7 — hook dispatch was implemented after this audit snapshot.**
  The extension manager now exposes a bounded `/v1/hooks/dispatch` invocation
  path with endpoint validation, timeout, fallback policy and a three-failure
  circuit breaker; the autonomous control plane invokes `pre_turn` and
  `post_turn` lifecycle hooks when configured. Full hook coverage for every
  lifecycle event and production extension endpoint deployment remain to be
  exercised in integration tests.
- **§32 — cross-service metric-emission breadth.** The observability
  service's own ingestion/correlation/export contract is now complete
  (this pass), but only `transparency-projection` actually pushes metrics
  into it today; other services (e.g. `backup-manager`) do not yet emit
  their §32-listed metrics (backup age, restore status, etc.). Instrumenting
  every producing service is a genuine, multi-service rollout gap outside
  this pass's assigned service boundaries, not something the observability
  service itself can close.
- Everything marked **PARTIAL (Carried)** or **FAIL (Carried)** in the
  master table above (sections 1, 2, 3, 5, 6, 7, 17, 18, 24, 25, 26, 27, 36,
  37) — none of these were in this pass's assigned scope and none should be
  read as newly-confirmed PASS. They stand exactly as the most recent prior
  audit (`governance-audit-full.md`, 2026-07-27) described them until a
  future pass re-verifies them against current code.

## Final Build/Test Verification (this pass)

- `go build ./...` — **PASS, clean, verified directly against the current
  tree while writing this document** (`go build ./...` with the doc's own
  Bash tool call, exit code 0, no output). Note for the record: at one
  point mid-session, the `native-planes` group's isolation check (`git
  stash` + rebuild) found `services/execution-proxy/cmd/server` failing on
  `s.applyRewritesForMode undefined` and recorded that as a pre-existing,
  out-of-scope defect. By the time this document was written, that symbol
  is defined (`services/execution-proxy/cmd/server/main.go:979`,
  `applyRewritesForMode`) — the `native-planes` group's own §20.9 fix
  introduced it as part of adding the required safety-mode helpers, and the
  full-repo build was independently re-confirmed clean here rather than
  taking any single group's snapshot on faith.
- `go test ./...` — PASS against the current tree after updating the stale
  memory-intelligence tests to the database-backed implementation.
- `python3 -m pytest tests/ -q` — PASS (17 passed).
- `python3 -m unittest discover -s workers/consolidation/tests` /
  `cd workers/consolidation && python3 -m pytest -q` — PASS (47 passed).
- `python3 scripts/validate_json.py` — PASS (6 schemas validated).
- `python3 scripts/audit_governance.py` — PASS ("structural gate only").
- `python3 scripts/audit_governance_runtime.py` — PASS.
- No merge-conflict markers or migration-numbering collisions found across
  `database/migrations/*.sql` (000001-000011, unique sequential prefixes).

## Addendum: Follow-Up Fixes (Stub/Placeholder Sweep)

A follow-up pass specifically hunted for mock data, stub handlers, and
fabricated telemetry left in production code paths (a repo-wide grep for
`TODO|FIXME|placeholder|stub|dummy|not implemented` turned up nothing beyond
test-double names and the audit script's own detection regex — this sweep
instead looked for functionally-dead handlers and hardcoded status values by
reading the code directly). It found and fixed two real remaining
instances of fabricated/non-functional operational state:

- **`services/memory-intelligence/cmd/server/main.go`, `handleDashboardSystem`**
  (§25.9 System Map). Previously returned a fixed literal list of 8 services
  all reporting `Status: "unknown"`, `Version: "unknown"`, `Uptime:
  "unavailable"` — honest in tone but functionally a dead stub that never
  queried anything. Replaced with real concurrent HTTP probes: each of the 7
  peer services' `/health/ready` and (newly added, see below) `/v1/version`
  endpoints are called with a 5s timeout using `tlsserve.TrustedClient`, and
  the entry reports the actually-observed `healthy`/`degraded`/`unreachable`
  status, real version string, and real uptime duration. The
  `memory-intelligence` self-entry reports its own real in-process uptime and
  version directly rather than probing itself over the network. Peer base
  URLs resolve via `SCOS_<SERVICE>_URL` env vars with compose-matching
  defaults, mirroring the exact pattern already used by
  `transparency-projection`'s `resolveUpstreams()`. `TestDashboardSystem` was
  rewritten to assert the self-entry is real and that peer entries are never
  fabricated (an unreachable peer must report `version: "unknown"`, never a
  fixed placeholder value).
- **`services/compatibility-gateway/cmd/server/main.go`, adapter
  `HealthStatus`**. All 10 registered adapters hard-coded
  `HealthStatus: "healthy"` at registration time with no code path anywhere
  in the file that ever updated it — an unconditional, unverified claim of
  health, which is a direct instance of the §42 prohibition on fabricated
  operational state (arguably worse than reporting "unknown", since it
  actively asserts a false positive). Fixed by removing the hardcoded field
  and adding `adapterHealthStatus()`/`snapshotAdapter()`, which derive a real
  status per request: untested boundaries (§23.7) are always
  `experimental_untested` regardless of traffic; tested boundaries report
  `healthy` only if a real agent registration exists for that boundary with
  `Status: "active"` and a `LastSeen` timestamp within the last 24 hours,
  otherwise `no_recent_traffic`. `handleAdapters`, `handleAdapterByBoundary`,
  and `handleCompatibility` now return computed snapshots instead of the raw
  stored struct.

Both fixes required a real `GET /v1/version` endpoint to exist on the peer
services being probed; only `memory-gateway` had one. Added the same
`{status, version, uptime_seconds}` handler (unauthenticated, matching
`/health/live`'s tier) to `retrieval-orchestrator`, `context-optimisation`,
`code-intelligence`, `execution-proxy`, `extension-manager`,
`compatibility-gateway`, and `memory-intelligence`, each backed by a real
`time.Since(startTime)` uptime measurement rather than a fixed value. Added
the previously-missing `SCOS_COMPATIBILITY_GATEWAY_URL` env var (with a
compose-matching default) and wired all 7 peer URL env vars into
`memory-intelligence`'s compose environment block;
`docker compose ... config` and `go build`/`go test ./...` were re-verified
clean after these changes (see re-run below).

**Addendum verification:** `go build ./...` — PASS. `go test ./...` — PASS,
all 39 packages, including the rewritten `TestDashboardSystem` and the
unchanged `compatibility-gateway` suite (no test asserted the old hardcoded
`HealthStatus`, so none needed updating there).
`python3 -m pytest tests/ -q` — PASS (17). `python3 -m unittest discover -s
workers/consolidation/tests` — PASS (47). `python3 scripts/validate_json.py`
— PASS (6 schemas). `python3 scripts/audit_governance.py` /
`audit_governance_runtime.py` — both PASS. `docker compose -f
deploy/compose/docker-compose.yml config` — PASS with required secrets set.

**Bottom line:** this pass closed 15 concrete code-level gaps across events,
code intelligence, execution proxy, extensions, compatibility, multi-agent
registration, crash recovery, backup verification, and observability
correlation — each with real fixes and tests, not just findings, and the
full repository builds and tests clean as of this document. It did **not**
re-verify roughly a third of GOVERNANCE.md (carried sections above) and does
**not** claim those are fixed or complete. The repository is closer to
governance-complete than either prior document alone suggested for the
sections re-audited here, but it is still **not** fully governance-complete:
genuine deployment-owned gaps (real TLS/PKI, off-host storage, a live OTEL
collector, production key management) and genuine remaining code work
(canary installs, cross-service metric rollout, complete lifecycle-hook
coverage and every carried-forward section) remain.
