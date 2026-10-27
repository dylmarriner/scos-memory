# P5 unified historical code readback (development evidence, 2026-10-28)

## Contract

`POST /v1/retrieve` and MCP `scos_retrieve` accept optional `as_of_commit`
and `snapshot_id` selectors. Either selector requires a durable `repository_id`
and explicit `branch`; both may be supplied to bind a particular generation.
The code lane uses `POST /v2/code/query` with the authenticated tenant and
principal, never an active-branch fallback. Even memory-classified natural
language queries must execute the explicitly selected code lane.

Successful historical code readback includes `code.historical=true`, the
resolved `code.snapshot_id`, `code.commit_id`, `code.branch`, and concrete
`code.results[].SymbolVersionID`, `SnapshotID`, `CommitID`, `Branch` values.
The orchestrator rejects missing/mismatched response and per-result provenance.
An invalid selector returns a safe 400; an unknown snapshot returns a safe 404; an ambiguous commit returns a safe
409 and asks for `snapshot_id`. Other downstream errors, unavailable services,
and malformed provenance fail closed with 502, not a misleading empty/current
code result. Downstream error bodies are never exposed; diagnostics record
only the HTTP status or a fixed provenance-mismatch category.

`retrieval_runs.filters` records the requested immutable code selectors,
repository UUID and branch (even on an empty result), and
`code_temporal_scope=immutable_snapshot`; selected symbol-version IDs are
recorded separately. **Memory sections remain governed by the current SCOS
system belief** (and optionally `as_of_valid_time`). `as_of_commit` is not
system-time travel for memory. `as_of_system_time` remains explicitly 501
until a trustworthy historical belief ledger exists.

## Evidence and remaining gates

- TDD: new client/handler/schema tests failed before implementation and
  passed afterward. Separate HTTP tests cover trusted identity forwarding,
  missing repository/branch, memory-classified query forcing, mismatched
  top-level and per-result provenance, and safe 404/409/502 outcomes.
- Disposable PostgreSQL 17/pgvector, 80 ordered migrations: real retrieval
  run persistence confirms immutable selectors and selected symbol-version ID.
- Existing code-intelligence PostgreSQL historical-query regression passed:
  immutable old generation, tenant isolation, ambiguity, and branch binding.
- Full Go suite, vet, race, GitHub CI, PR review and merged-main verification
  must be recorded from actual results before treating this branch as merged.

This is a source-level integration slice, not complete P5. It does not prove
server-attested Git source provenance, historical memory approvals/statuses,
production rollout, recovery, representative performance or P0-P8 acceptance.
