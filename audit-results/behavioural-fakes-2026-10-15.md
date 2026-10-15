# Behavioural-fake audit — 2026-10-15

Scope: code that compiles, passes its tests and carries no TODO/FIXME/placeholder
marker, yet does not do what its name, API contract or response claims. A marker
grep had already come back clean; this audit looks for behaviour.

Method: four independent read-only auditors, each with a separate slice (data and
retrieval services; control and governance services; ops and edge services;
workers, packages, scripts, benchmarks and the dashboard), each told to try to
disprove every candidate by tracing callers and callees. These were Explore agents,
working in parallel, with no shared conclusions. Then the user flows were exercised
end to end through the MCP gateway on a deployed Compose stack (local mode),
the way an agent uses the fabric.

Status key: **FIXED** (fix committed, with a test), **OPEN** (confirmed, not yet
fixed), **VERIFY** (auditor claim not yet independently reproduced).

## User flows exercised through the MCP gateway

| Flow | Result before fixes | Root cause | Status |
|---|---|---|---|
| Any tool in local mode | 401 from every downstream service | Gateway resolved the local identity but forwarded no credentials | FIXED (gateway mints a signed local-identity token) |
| Memory save via turns | Fact parked in `pending_review` forever | "High-impact" substring keywords (`production`, `deploy`, …) plus no approve/reject path anywhere | FIXED: `scos_memory_review_list` / `scos_memory_review` approve or reject (rationale required to reject; single decision; duplicate approvals merge). Verified live: held fact approved, then recalled |
| Memory save via turns | Candidate is the last four raw chat messages, titled `turn/<uuid>` | Extractor copies transcript text instead of extracting the stated fact | FIXED: explicit statements ("remember…", "note…") are extracted as the memory; paraphrases collapse into one memory with merged evidence; consolidated memories carry network and source. Verified live |
| Memory save, direct | No MCP tool can save a memory | Only the turn journal writes memories | FIXED: `scos_remember`. Verified live: remember → recall → correct → explain → forget |
| Code index (path strings) | `status: active`, `symbol_count: 2`, then the query for an existing symbol returns nothing | Repository is not mounted in code-intelligence; v1 "indexes" paths it cannot read | FIXED: the workspace is mounted; `scos_code_index` scans it into a durable snapshot in the background (`scos_code_index_status`); unchanged trees reuse their snapshot. Verified live: 863 files, 10,654 symbols in ~21s; edits reindexed |
| Code impact | Always empty dependants, risk score from symbol count | Gateway routes to v1 `/v1/impact`, whose graph is never built | FIXED: query and impact use the durable v2 store; calls through imported packages resolve across packages; bare names accepted. Verified live: `SignClaims` impact lists its callers in three services |
| Graphify query/path/explain | 500 `mkdir /var/lib/graphify-cache/...: permission denied` | Image never creates the cache dir, so the named volume is root-owned | FIXED (deploy/Dockerfile) |
| Graphify query/path/explain (after the cache fix) | Extractor killed; gateway timeout | OOM under the shared 160m cap (`GRAPHIFY_SERVICE_MEM_LIMIT` no longer applied); a first build outlasts the 15s tool call | FIXED: own memory limit; background build with a 202 "building" answer; staged, atomic publish. Verified live on a workspace repo and the default repo |
| Graphify ingest into memory | Every write 401'd while the endpoint answered 200 | No credentials; fixed configured identity; bare labels merged distinct declarations | FIXED: writes as the calling principal in a background job (`scos_graphify_ingest`, `scos_graphify_ingest_status`); node facts carry their source location. Verified live |
| Recall | A fact answering the question ranked below code entities sharing one word | Relation expansion double-counted direct hits; rank fusion lost match strength; hermes flattened by section | FIXED. Verified live on four questions |
| Dashboard (local mode) | Empty panels | Minted for an invented tenant | FIXED: required identity, local overlay uses the local-mode user. Verified live |
| Governed command (`scos_control_route` + `scos_control_confirm`) | Never ran | Governance asked to authorise the command string; start never resolved built-in tools; proxy deployed without the governance URL, workspace mount or the workspace owner's uid; exit status ignored | FIXED. Verified live: `rm -rf ./build-output` held for confirmation, then ran in the repository |
| Permission check (`scos_permission_check`) | 401 always | Only gate-issued tokens accepted, which the middleware rejects | FIXED: verified claims identity accepted below secret; local user granted. Verified live |
| Recovery status after restart | Lost | Served from memory only | FIXED: reloaded from the database at startup. Verified live |
| Gateway error text | Every forwarding failure says "code-intelligence returned …" | Service name hard-coded in the forward helpers | FIXED |

## Findings by area

### Code intelligence
- v1 graph never populated (`Imports`, `OutboundRelations`, `InboundRelationSummary`, `Tests` are never assigned); `/v1/impact`, `/v1/context`, `reference_search` return empty; `/v1/adapters` advertises capabilities with no code. — OPEN (no MCP tool or dashboard panel reaches v1 query/impact any more; the v1 routes themselves are still served)
- v1 `SymbolCount` counts the global store across tenants (`main.go:805`); v2 stores file count as symbol count (`pgstore.go:261`). — FIXED (both count the snapshot's own symbols)
- v2 trusts the client-supplied `content_hash` without checking it against `content`. — FIXED (refused with 400 when it does not match)

### Retrieval orchestrator
- `memory_classes`, `include_recent`, `include_state`, `filters` accepted and ignored; `filters` recorded in `retrieval_runs` as if applied. — FIXED (real lanes and class filter; `filters` refused; applied restrictions recorded)
- Exact/full-text/relation/session lane failures only logged; response has no degraded field, so a DB failure reads as "no matches". — FIXED (`degraded_sources`/`degraded_models`, passed through recall)
- `assembleSections` overwrites lane provenance with a class label and that label is persisted as the audit source. — FIXED (provenance is the lanes that found the memory)
- Standing-answer shortcut skips the sensitivity filter and the run record. — VERIFY

### Context optimisation
- Hard-coded list of 14 transformations returned for every profile (8 unimplemented); ranges and token accounting constant; `source_hash_recorded` literal true. — FIXED: only transformations applied to the input are reported, with source byte ranges and before/after tokens (one estimate, `utf8_bytes_div_4_ceil`); included/excluded ranges partition the source; `source_hash_recorded` reads the source back from the store
- Pass-through segments count 0 compiled tokens, inflating reduction ratio; restoration and exact-value metrics are constants. — FIXED: compiled tokens are measured on the returned text (headers included); exact-value and instruction retention are retained/total ratios; restoration success is a real restore of the capsule just written
- Model, session context, code symbols, memory references and output intent accepted and ignored; output `capsule_id` is random and unrestorable. — FIXED: `model`, `session_context`, `request_intent` (and unknown profiles, `diff` format) rejected with 400; code symbols and memory references are verified retention pins; input and output capsule ids are content-derived, recorded in a manifest and restorable via `/v1/context/restore {"capsule_id"}`
- Readiness checks `os.TempDir()` instead of the source store. — FIXED: readiness writes to the configured source store

### Memory intelligence
- `/v1/reflect` beliefs are facts scaled by 0.9; "stale" means "agrees with its evidence". — FIXED: candidate beliefs group agreeing evidence and take confidence from combined support less contradicting evidence (inferred facts never count); a held belief is stale only when cited evidence is missing, withdrawn, expired, revised after the belief's last revision, or contradicted by a fact recorded since, each with its reason. Also fixed: every belief write failed (`text = uuid` in the evidence lookup)
- Relation confidence constant 0.8; hierarchy `fact_count` taken from the request body. — FIXED: confidence is the caller's (validated 0–1) or the weaker of the two facts' confidences; contradictions, merges and corrections carry their own measure; graphify sends its edge score. Hierarchy nodes name their facts (`fact_ids`, migration 000053) and `fact_count` counts the active facts in the subtree on read; a supplied count is refused
- Diary writer never called; system dashboard probes peers without a token; code-symbol links dropped below thresholds but echoed back. — FIXED: the diary is written in the same transaction as each fact record, correction, forget, belief and review decision; peer version probes carry the service token and control-plane token and report a refusal; a fact write echoes only links actually recorded and lists the rest under `code_symbol_links.dropped` with the reason
- Entity-link errors inside the fact transaction abort the commit despite "best-effort" comment. — VERIFY

### OKF synchroniser
- Proposer, reviewer and role taken from the request body, not the authenticated caller: any service-token holder can approve their own proposal as a named curator. — FIXED (security): identity from claims, roles from `okf_governance_roles`, no self-review. Merge itself still has no authorisation of its own — OPEN
- Divergence detection compares a value with itself. — FIXED (also merge-time path matching)

### Autonomous control plane
- Capability selection is recorded but never shapes the executed plan. — FIXED: each capability-backed step (memory, code, context, governance, proxy, recovery, observability) runs only if its capability was not rejected; a rejected one is reported skipped with the rejection reasons and no peer is called. A proposed command now grants `execution:invoke`, so the proxy is no longer recorded "unauthorised" for every governed command.
- Four "required" stages are marked completed with no work done. — FIXED (each verifies its work or is reported skipped with the reason)
- Recovery can never trigger (`running`/`interrupted` never written); recovery step has no service URL. — FIXED for runs (persisted state, `/v1/control/recover`); the recovery-orchestrator plan step is reported skipped
- Sends `confirmed: !ConfirmationRequired` to the execution proxy, asserting user confirmation that never happened; governance failure does not stop execution. — FIXED (security): recorded confirmation via `/v1/control/confirm`; governance fails closed. The proxy still trusts `confirmed` from any control-plane-token holder — FIXED (security): the proxy runs under the operation the control plane started and acts only on the confirmation tool governance recorded for it (prepare claims that operation once, for its own principal and command, migration 000055); the request's `confirmed` authorises nothing. Verified live: a direct `rm -rf ./x` with `confirmed: true` is refused 403; after `/v1/control/confirm` it runs and the operation is reconciled.
- Persisted decisions drop scope and capability data. — FIXED: decisions now carry a resolved scope; scope, selected capabilities, reserved fallbacks and rejected capabilities (with reasons) are written to `resolved_scope`, `candidate_capabilities` and `rejected_alternatives` and read back, so a confirmed or recovered run is gated by the same selection. Classified-label confidence and evidence are still not persisted.

### Tool governance / execution proxy / security gate / credential vault
- Prepare auto-registers and authorises any tool; plan and risk dropped by the decoder. — FIXED (security): prepare refuses unknown tools; registration and authorisation need the `tools:admin` scope; execution-proxy's built-in tool is seeded by migration 000047; plan and risk are decoded, re-graded by the shared classifier, persisted on `tool_executions`, and destructive risk requires confirmation.
- Declared tool properties never enforced; reconcile state machine partly dead. — FIXED: read_only, destructive, externally_side_effecting, dry_run_capable, confirmation_gated and idempotent are enforced at prepare/start, reconcilable at reconcile; execution-proxy now reconciles as the preparing identity (it sent none, so every reconcile was refused).
- Rewrite rules match on the first word and apply only the first replacement word; `cat`→`head` is lossy under "lossless-only". — FIXED: rules are regular expressions applied in full (capture groups, rest of the command kept, `unless` guard, risk-class match); each declares `lossless`, safe mode applies only lossless rules, and `cat`→`head -n 100` is lossy. Registered rules are validated.
- Diagnostic-retention metric compares stderr with itself. — FIXED: it compares the distinct error and warning lines of raw stdout and stderr with what the delivered result carries verbatim.
- Permission policy allows every non-secret class and ignores action, resource, delegation, expiry; unknown classes allowed. — FIXED (security): grants and delegations (000048), unknown classes denied, reasons recorded
- Secret resolve ignores scope and data class; audit record accepts caller-chosen fields. — FIXED (security): scope and clearance enforced and denials audited; caller fields kept only as an annotation
- Vault "rotate by restart" would make every credential unreadable; `key_version` never used. — FIXED (keyring with retired keys; audited re-seal)

### Agent registry / compatibility gateway / extension manager
- Handoff responses coerced to rejected; expiry never applied; responder not checked; declared permissions never enforced. — FIXED (only `accepted`/`rejected` accepted, else 400; answered once, only by the addressed agent's token or the principal that registered it, from claims; responder/time/reason recorded; expired handoffs refused with 410 and stored `expired`; namespaces and `required_tool_permissions` checked against the receiver's declared memory/tool permissions at initiate and accept; migration 000054; integration-tested as scos_app)
- Compatibility matrix hard-codes `Tested: true` with no conformance suite; `session_watcher` has no implementation. — FIXED (`tested` is derived from an in-process conformance run over each boundary's translate and render paths, reported at `/v1/compatibility/conformance`; the run caught text render dropping the user model, now fixed; `session_watcher` is `support: unsupported`, all operations unsupported, registration refused)
- Translate ignores `target_format`; render falls back to JSON while echoing the requested format. — FIXED (real openai/anthropic/mcp/scos_common codecs via a common event model; unsupported formats 400, events the target cannot express 422; render returns the format it produced and refuses unknown formats)
- Isolation modes, content hash, hook semantics, enable/disable and canary are labels only. — FIXED (only `remote_endpoint` accepted and reported, with what it enforces; `content_hash` is sha256 of the manifest, verified on install/enable/rollback/promote/load; hooks report the dispatcher's real type/retry/timeout; disabled extensions get no hook deliveries and their skills are unavailable; canary routes hook dispatch to the cohort)
- Uninstall deletes other extensions' hooks and leaves its own skills active. — FIXED (removes exactly that extension's hooks, skills, canary and history; refused while an enabled extension depends on it; skills left by the old uninstall are dropped at load)

### Consolidation worker / index maintenance
- "Merged" duplicates are promoted as a second active memory; evidence merge reads a field never loaded. — FIXED
- "Superseded" logic inverted; higher-authority duplicate silently dropped. — FIXED
- Conflict sets written to ids that are never candidates. — FIXED: subjects are the candidate id and the existing memory or pending candidate id; the set is appended to the candidate's `conflict_assessment` and the memory's metadata, and a subject matching no row fails the job
- Embedding backlog ignores embedding spaces. — FIXED: the embeddings worker records its space as the active one (migration 000053); the backlog counts active memories with no vector in that space, and index maintenance reports which space it measured

### Benchmarks
- `--live` discards the live run's results and drops the "live evidence not included" note. — OPEN
- Context-optimisation benchmark echoes ground truth and passes by construction. — OPEN
- Proxy `raw_result_restoration` always 1.0; `token_reduction` truncates the command string. — OPEN
- `cost_usd` is invented. — OPEN
- Code-intelligence candidate is an index reimplemented inside the script; the service is never called. — OPEN
- Deployed benchmark splits total wall time evenly as per-case latency; branch isolation test passes on any result. — OPEN
- Harness metrics `answer_regression`, `promotion_*`, `citation_accuracy` hard-wired; `--suite` crashes. — OPEN

### Packages, scripts, integrations
- Python agent SDK returns a fabricated envelope with granted permissions on any error. — FIXED: every failure (unreachable, non-2xx, bad JSON, not an envelope, wrong identity) raises `EnvelopeRenderError`; no local envelope or permissions are ever made
- Python Hermes provider `flush()` discards the queue and reports it as drained. — FIXED: `flush()` runs prefetch work (served by the next matching `prefetch`) and hands turns to an `ingester`; anything undelivered stays queued and persisted, and is returned with its reason
- `GoAdapter` "AST parsing" is a line-prefix scan. — FIXED: parses with `go/parser`; functions, methods, types with full line spans; invalid source is an error
- Integration doctor marks claude-code and codex VERIFIED on any machine. — FIXED: VERIFIED now also requires, on this machine, the binary, the SCOS plugin/MCP entry in the agent's config, and an MCP `tools/list` answered by the gateway that config points at; failed checks are listed
- `verify_definition_of_done.py` passes items by test name existing. — VERIFY
- OpenClaw `minScore` ignored. — FIXED: recall results carry SCOS's fused score and `memory_search`/`scos_recall` drop those below `minScore` (unscored results too)

### Fixed in this pass, outside the user flows
- Four compiled service binaries (10-29MB) were committed; untracked and ignored.
- tunnel-client local proxy answered non-POST MCP with 501 instead of 405. — FIXED
- tunnel-client e2e package never compiled: its PEM fixtures were excluded by `*.pem` ignore. — FIXED (fixtures generated per run)

### Observability, transparency, gateway, hermes, cloud, backup, recovery, graphify, model runtime
- Dashboard default identity is a placeholder tenant (`11111111-…`) nothing writes to, so a local stack's dashboard shows none of the user's data. — FIXED (also: the code panel now shows the durable index)
- Graphify `/extract` ingestion sends no credentials to memory-intelligence: every node and edge 401s while the endpoint answers 200. — FIXED
- Backup `VerifiedAt` is stamped at read time, so the backup-age alert can never fire (introduced by this branch's backup work). — FIXED (age measured from when the backup was taken)
- Backup plan coverage flags hard-coded or env-asserted; off-host is a local write; no WAL or git-mirror code; `BackupProvider` has no implementation. — OPEN
- Retention never applied to production backups. — OPEN
- Restore-drill types are labels; one restore satisfies every cadence; drill validates no data. — OPEN
- Recovery checklist steps 4 and 9 report work that is not done; event "replay" is a flag flip. — OPEN
- Recovery status served from an in-memory map; database never read back, so status is lost on restart. — FIXED (000050)
- Hermes prefetch queue never consumed; `cache_hit` invented; shutdown `replayable` constant; `save_config`/`initialize` do nothing; sync_turn returns the caller's turn id instead of the stored one and drops tool calls. — FIXED (000056): a worker consumes the queue (session turns + governed recall as the requester); `cache_hit` is true only when that result is served and still current; shutdown cancels and reports the caller's pending work and computes `replayable`; `save_config` stores per principal/profile scope and `initialize` binds the profile and returns it; sync_turn returns the stored turn id (plus `client_turn_id`) and stores tool calls on the turn. Verified against PostgreSQL as `scos_app`
- Gateway: `scos_auth_issue` can never work (issuer-secret header not forwardable); backup/recovery admin tools always 403 on the hosted path; `scos_benchmarks` description promises runs it cannot do; `scos_recall` session id unused; error text names the wrong service. — Error text FIXED; local mode now carries the backup/recovery admin scopes. The rest OPEN
- Cloud plan tiers promise quotas and feature gating nothing enforces. — OPEN
- Observability restart discards up to 15s of acknowledged, fsynced records. — FIXED: journal records are sequenced; snapshot and rollups are checkpointed in one transaction with the journal position, and a restart replays every later journal record into the ring and rollups. Verified against PostgreSQL as `scos_app`
- Model runtime reports configured model labels, not served ones; readiness ok with no backend. — VERIFY

## Limits of this audit
- Each auditor lists files it did not reach in depth; those are not covered.
- OPEN items are confirmed by the auditor's trace, not yet independently reproduced
  by a second reviewer except where a user flow reproduced them above.
