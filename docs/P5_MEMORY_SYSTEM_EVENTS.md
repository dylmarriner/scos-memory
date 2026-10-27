# P5 memory system-state transition evidence (merged source; production not qualified)

Migration `000082_memory_system_events.sql` is an **instrumentation prerequisite**, not an implementation of bitemporal retrieval. It captures state-bearing changes to `memory_items` in the same transaction as each mutation. Existing rows receive an explicitly marked migration-time `baseline` event, never a fictitious historical creation timestamp. An insert and a meaningful state update each produce a separate event. Hard deletion is not instrumented until offboarding/cascade recovery is qualified. Unrelated metadata updates do not claim a belief-state change.

Each event contains tenant, memory, namespace and owner IDs, type/status, selected immutable version and supersession references, authority/confidence, sensitivity and valid-time bounds. It deliberately excludes content and structured metadata. A database-owned trigger writes events; application roles cannot insert forged events or update/delete history. FORCE RLS requires current access to the memory plus historical-namespace membership and historical sensitivity. Deleted memories are not visible through the runtime read policy. Tenant offboarding requires a privileged, reasoned call to `purge_memory_system_events_for_tenant(tenant_id, reason)` before tenant deletion. The procedure locks the tenant, records a durable append-only purge audit with operator, count and transaction ID, deletes exactly that count, and refuses unaudited cascades. The purge audit intentionally survives tenant deletion; event history does not. This table is not an immutable external audit archive.

**Temporal limitations:** `observed_at` is a database clock observation, **not a commit timestamp**. A transaction can roll back, concurrent transactions can commit out of observation order, and events predating this migration do not exist. Neither the row's `created_at` nor its first version can safely reconstruct earlier approval, revocation or supersession state. Memory content redactions, version approvals, ACL changes, commit-order serialization, hard-delete/offboarding capture, deleted-memory recovery and point-in-time replay remain outside this increment. `POST /v1/retrieve` continues to return HTTP 501 for `as_of_system_time`. Do not treat these rows as sufficient evidence to answer historical-belief questions.

## TRUNCATE integrity hardening (28 October 2026; merged PR #106)

A real PostgreSQL 17 regression demonstrated that the migration owner could
`TRUNCATE memory_system_events` and `TRUNCATE memory_system_event_purge_audit`
without firing the existing row-level DELETE guards. Both commands succeeded
before the fix (RED). Migration `000083_memory_system_history_truncate_guard.sql`
adds statement-level `BEFORE TRUNCATE` guards to both tables, including
a `TRUNCATE tenants CASCADE` attempt. Both now fail with SQLSTATE `55006` (GREEN,
five repeated runs). The privileged, reasoned, count-checked per-tenant DELETE
purge remains available; the guard does not turn ordinary offboarding into
an unsupported operation. The full Go suite (51 packages), 90 Python regression tests,
full retrieval PostgreSQL integration, `go vet ./...` and focused race tests
passed against a disposable database with all 83 migrations and runtime
policies. This is source-only data-loss hardening; the production migration,
recovery qualification, historical commit ordering and `as_of_system_time`
HTTP 501 gate remain outstanding. PR #106 merged with expected head
`b2fdfbaea119b4fd3226cdbf0462297080af3253` into main
`f0d0b8b39260dedf744293de9775e246298b53e9`. Both merged-main CI
(5/5 jobs) and P4 PostgreSQL integration (9/9 jobs) passed on that SHA.
This is not production rollout or recovery qualification.

## Acceptance evidence and rollout boundary

The regression `TestP5MemorySystemEventsAreAppendOnlyAndTenantIsolated` runs against real PostgreSQL 17/pgvector with restricted `scos_app` role. It verifies insert and status-change events, no forged or mutable events, tenant isolation, unchanged metadata not generating false events and no connection-context leakage. Production rollout, backfill scale, lock duration, full offboarding/recovery, redaction and commit-order semantics require separate qualification before enabling a historical query path. **Source merge is not authorization to run migration `000082` on a live database.**

**Local isolated qualification, 28 October 2026:** PostgreSQL 17/pgvector with TLS, all 82 numbered migrations and runtime policies applied to a disposable database. The regression first failed against pre-`000082` schema because the event table did not exist, then passed five repeats and two race-enabled repeats on the final migration. The restricted role was deliberately granted broad DML privileges in the test fixture: its UPDATE was denied by FORCE RLS with **zero rows affected**, not a SQL error, and the test was corrected to assert that precise outcome. Full uncached Go suite passed (51 packages), `go vet` passed, and 83 Python script regressions passed. The §41 definition-of-done checker still fails with PARTIAL governance evidence. Production upgrade, offboarding and recovery remain unqualified.

**Review hardening:** Codex identified two material issues on PR #103: an unaudited FK cascade would be blocked by the append-only trigger, and concurrent writes could fall between the baseline snapshot and trigger installation. The migration now supplies a privileged audited tenant-purge path and holds `SHARE ROW EXCLUSIVE` on `memory_items` through baseline and trigger creation. A real PostgreSQL purge regression verifies runtime denial, unauthorized cascade rejection, same-transaction audited tenant deletion, rollback restoring events and audit, and immutable audit rows. A source-order test failed RED before the lock fix and passed GREEN afterward; a separate real PostgreSQL concurrency test confirmed the lock blocks an INSERT until released, after which its event is captured. Focused PostgreSQL tests passed twice with Go race detection; the full uncached Go suite passed (51 packages), go vet passed, and 83 Python script regressions passed on a separate disposable PostgreSQL 17/pgvector database with all migration and policy SQL applied. Production offboarding must still be qualified end-to-end.

**Additional CodeRabbit review qualification (28 October 2026):** The migration-owner FORCE-RLS prerequisites were already introduced by ordered migrations `000064` (owner SELECT on `memory_items`) and `000071` (owner SELECT/UPDATE on `tenants`). Migration `000082` now documents that dependency, and a real PostgreSQL catalog regression asserts all three owner-only policies and FORCE RLS remain present. The forged-event test now supplies every NOT NULL field and requires SQLSTATE `42501`; an isolated mutation test temporarily granted `scos_app` INSERT and a permissive policy, causing the test to fail RED with a successfully forged event, then restored the restricted policy and passed GREEN. This does not establish production upgrade or offboarding qualification.
