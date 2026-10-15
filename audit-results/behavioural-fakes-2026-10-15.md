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
| Memory save via turns | Fact parked in `pending_review` forever | "High-impact" substring keywords (`production`, `deploy`, …) plus no approve/reject path anywhere | OPEN |
| Memory save via turns | Candidate is the last four raw chat messages, titled `turn/<uuid>` | Extractor copies transcript text instead of extracting the stated fact | OPEN |
| Memory save, direct | No MCP tool can save a memory | Only the turn journal writes memories | OPEN |
| Code index (path strings) | `status: active`, `symbol_count: 2`, then the query for an existing symbol returns nothing | Repository is not mounted in code-intelligence; v1 "indexes" paths it cannot read | OPEN |
| Code impact | Always empty dependants, risk score from symbol count | Gateway routes to v1 `/v1/impact`, whose graph is never built | OPEN |
| Graphify query/path/explain | 500 `mkdir /var/lib/graphify-cache/...: permission denied` | Image never creates the cache dir, so the named volume is root-owned | FIXED (deploy/Dockerfile) |
| Gateway error text | Every forwarding failure says "code-intelligence returned …" | Service name hard-coded in the forward helpers | OPEN |

## Findings by area

### Code intelligence
- v1 graph never populated (`Imports`, `OutboundRelations`, `InboundRelationSummary`, `Tests` are never assigned); `/v1/impact`, `/v1/context`, `reference_search` return empty; `/v1/adapters` advertises capabilities with no code. — OPEN
- v1 `SymbolCount` counts the global store across tenants (`main.go:805`); v2 stores file count as symbol count (`pgstore.go:261`). — OPEN
- v2 trusts the client-supplied `content_hash` without checking it against `content`. — OPEN

### Retrieval orchestrator
- `memory_classes`, `include_recent`, `include_state`, `filters` accepted and ignored; `filters` recorded in `retrieval_runs` as if applied. — OPEN
- Exact/full-text/relation/session lane failures only logged; response has no degraded field, so a DB failure reads as "no matches". — OPEN
- `assembleSections` overwrites lane provenance with a class label and that label is persisted as the audit source. — OPEN
- Standing-answer shortcut skips the sensitivity filter and the run record. — VERIFY

### Context optimisation
- Hard-coded list of 14 transformations returned for every profile (8 unimplemented); ranges and token accounting constant; `source_hash_recorded` literal true. — OPEN
- Pass-through segments count 0 compiled tokens, inflating reduction ratio; restoration and exact-value metrics are constants. — OPEN
- Model, session context, code symbols, memory references and output intent accepted and ignored; output `capsule_id` is random and unrestorable. — OPEN
- Readiness checks `os.TempDir()` instead of the source store. — OPEN

### Memory intelligence
- `/v1/reflect` beliefs are facts scaled by 0.9; "stale" means "agrees with its evidence". — OPEN
- Relation confidence constant 0.8; hierarchy `fact_count` taken from the request body. — OPEN
- Diary writer never called; system dashboard probes peers without a token; code-symbol links dropped below thresholds but echoed back. — OPEN
- Entity-link errors inside the fact transaction abort the commit despite "best-effort" comment. — VERIFY

### OKF synchroniser
- Proposer, reviewer and role taken from the request body, not the authenticated caller: any service-token holder can approve their own proposal as a named curator. — OPEN (security)
- Divergence detection compares a value with itself. — OPEN

### Autonomous control plane
- Capability selection is recorded but never shapes the executed plan. — OPEN
- Four "required" stages are marked completed with no work done. — OPEN
- Recovery can never trigger (`running`/`interrupted` never written); recovery step has no service URL. — OPEN
- Sends `confirmed: !ConfirmationRequired` to the execution proxy, asserting user confirmation that never happened; governance failure does not stop execution. — OPEN (security)
- Persisted decisions drop scope and capability data. — OPEN

### Tool governance / execution proxy / security gate / credential vault
- Prepare auto-registers and authorises any tool; plan and risk dropped by the decoder. — FIXED (security): prepare refuses unknown tools; registration and authorisation need the `tools:admin` scope; execution-proxy's built-in tool is seeded by migration 000047; plan and risk are decoded, re-graded by the shared classifier, persisted on `tool_executions`, and destructive risk requires confirmation.
- Declared tool properties never enforced; reconcile state machine partly dead. — FIXED: read_only, destructive, externally_side_effecting, dry_run_capable, confirmation_gated and idempotent are enforced at prepare/start, reconcilable at reconcile; execution-proxy now reconciles as the preparing identity (it sent none, so every reconcile was refused).
- Rewrite rules match on the first word and apply only the first replacement word; `cat`→`head` is lossy under "lossless-only". — OPEN
- Diagnostic-retention metric compares stderr with itself. — OPEN
- Permission policy allows every non-secret class and ignores action, resource, delegation, expiry; unknown classes allowed. — OPEN (security)
- Secret resolve ignores scope and data class; audit record accepts caller-chosen fields. — OPEN (security)
- Vault "rotate by restart" would make every credential unreadable; `key_version` never used. — OPEN

### Agent registry / compatibility gateway / extension manager
- Handoff responses coerced to rejected; expiry never applied; responder not checked; declared permissions never enforced. — OPEN
- Compatibility matrix hard-codes `Tested: true` with no conformance suite; `session_watcher` has no implementation. — OPEN
- Translate ignores `target_format`; render falls back to JSON while echoing the requested format. — OPEN
- Isolation modes, content hash, hook semantics, enable/disable and canary are labels only. — OPEN
- Uninstall deletes other extensions' hooks and leaves its own skills active. — OPEN

### Consolidation worker / index maintenance
- "Merged" duplicates are promoted as a second active memory; evidence merge reads a field never loaded. — OPEN
- "Superseded" logic inverted; higher-authority duplicate silently dropped. — OPEN
- Conflict sets written to ids that are never candidates. — OPEN
- Embedding backlog ignores embedding spaces. — OPEN

### Benchmarks
- `--live` discards the live run's results and drops the "live evidence not included" note. — OPEN
- Context-optimisation benchmark echoes ground truth and passes by construction. — OPEN
- Proxy `raw_result_restoration` always 1.0; `token_reduction` truncates the command string. — OPEN
- `cost_usd` is invented. — OPEN
- Code-intelligence candidate is an index reimplemented inside the script; the service is never called. — OPEN
- Deployed benchmark splits total wall time evenly as per-case latency; branch isolation test passes on any result. — OPEN
- Harness metrics `answer_regression`, `promotion_*`, `citation_accuracy` hard-wired; `--suite` crashes. — OPEN

### Packages, scripts, integrations
- Python agent SDK returns a fabricated envelope with granted permissions on any error. — OPEN
- Python Hermes provider `flush()` discards the queue and reports it as drained. — OPEN
- `GoAdapter` "AST parsing" is a line-prefix scan. — OPEN (unused)
- Integration doctor marks claude-code and codex VERIFIED on any machine. — OPEN
- `verify_definition_of_done.py` passes items by test name existing. — VERIFY
- OpenClaw `minScore` ignored. — OPEN

### Fixed in this pass, outside the user flows
- tunnel-client local proxy answered non-POST MCP with 501 instead of 405. — FIXED
- tunnel-client e2e package never compiled: its PEM fixtures were excluded by `*.pem` ignore. — FIXED (fixtures generated per run)

### Observability, transparency, gateway, hermes, cloud, backup, recovery, graphify, model runtime
- Dashboard default identity is a placeholder tenant (`11111111-…`) nothing writes to, so a local stack's dashboard shows none of the user's data. — OPEN (user flow)
- Graphify `/extract` ingestion sends no credentials to memory-intelligence: every node and edge 401s while the endpoint answers 200. — OPEN (user flow)
- Backup `VerifiedAt` is stamped at read time, so the backup-age alert can never fire (introduced by this branch's backup work). — OPEN
- Backup plan coverage flags hard-coded or env-asserted; off-host is a local write; no WAL or git-mirror code; `BackupProvider` has no implementation. — OPEN
- Retention never applied to production backups. — OPEN
- Restore-drill types are labels; one restore satisfies every cadence; drill validates no data. — OPEN
- Recovery checklist steps 4 and 9 report work that is not done; event "replay" is a flag flip. — OPEN
- Recovery status served from an in-memory map; database never read back, so status is lost on restart. — OPEN
- Hermes prefetch queue never consumed; `cache_hit` invented; shutdown `replayable` constant; `save_config`/`initialize` do nothing; sync_turn returns the caller's turn id instead of the stored one and drops tool calls. — OPEN
- Gateway: `scos_auth_issue` can never work (issuer-secret header not forwardable); backup/recovery admin tools always 403 on the hosted path; `scos_benchmarks` description promises runs it cannot do; `scos_recall` session id unused; error text names the wrong service. — OPEN
- Cloud plan tiers promise quotas and feature gating nothing enforces. — OPEN
- Observability restart discards up to 15s of acknowledged, fsynced records. — OPEN
- Model runtime reports configured model labels, not served ones; readiness ok with no backend. — VERIFY

## Limits of this audit
- Each auditor lists files it did not reach in depth; those are not covered.
- OPEN items are confirmed by the auditor's trace, not yet independently reproduced
  by a second reviewer except where a user flow reproduced them above.
