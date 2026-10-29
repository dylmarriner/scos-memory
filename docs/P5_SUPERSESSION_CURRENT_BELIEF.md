# P5 current-belief supersession exclusion

**Status:** Source candidate, 2026-10-29. Not deployed, not historical belief reconstruction, not P5 or §41 completion.

## Defect and contract

Current-system-belief retrieval previously selected `memory_items.status='active'` without excluding rows whose `superseded_by_id` was already populated. A legacy/direct writer can leave the status active while recording a successor, and the database accepts that state. The read path then treated the superseded source as current evidence. Conflict projections also exposed its UUID as a live contradictory fact, including through the public HTTP response.

For the **current-belief** memory lanes, a source is eligible only when it has the appropriate status **and** `superseded_by_id IS NULL`. The same predicate applies to the other endpoint of a projected conflict relation. The rule covers exact, full-text, vector, recent-memory, task-state, session-state and relation-expansion retrieval. It is applied in SQL before ranking and serialization, not only in the final HTTP response.

The session-state lane still permits unsuperseded `candidate` rows belonging to the selected session. This change does not rewrite status, supersession links, historical events, or authorization policies.

## RED / GREEN evidence

`TestP5SupersededActiveMemoryExcludedFromCurrentRetrieval` seeds a disposable PostgreSQL 17/pgvector database with an active source, an active superseded memory, and its active replacement, plus versioned embeddings, session metadata and contradiction relations. The test checks seven retrieval lanes and the public HTTP response with both the database owner and non-BYPASSRLS `scos_app` runtime role. It requires the current source and replacement to remain readable, the superseded fact to be absent, and the source's only projected conflict to point to the replacement.

Before the SQL correction, the regression failed in all seven lanes and the HTTP response, including under the restricted runtime role. It exposed the superseded fact as both a result and a conflict target. After adding the source and conflict-target predicates, the focused regression passed five repetitions. Two race-enabled repetitions, the complete retrieval-orchestrator tagged PostgreSQL integration suite, the 52-package Go suite and `go vet ./...` passed. The §41 definition-of-done checker still reports FAIL because its master governance row remains PARTIAL.

## Remaining limits

This is a **current-system-belief** correction, not `as_of_system_time` reconstruction. It does not establish transaction commit order, reconstruct historical approvals or deletions, attest Git provenance, or qualify representative latency. The HTTP 501 historical-system-time refusal must remain in place until its independent acceptance gates are met.

No live database migration, production deployment, credit spending, or service changes were performed. The test used a separately named disposable database container.
