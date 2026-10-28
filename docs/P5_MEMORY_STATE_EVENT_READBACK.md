# P5 forward-only memory-state event readback

**Status:** Merged source in PR #119 (main `9ef2b438a9be833f725e7fac75aa82fda3c5e1cd`); exact-head and merged-main CI/P4 integration passed on 2026-10-28. Not deployed. This is **not** `as_of_system_time` reconstruction.

## Contract

`GET /v1/memory-state-events?memory_id=<UUID>&limit=20&after_id=0` is an authenticated, read-only endpoint on retrieval-orchestrator. It uses the existing authenticated tenant/principal identity, not client-supplied identity query fields. `limit` is 1–100 (default 20); `after_id` is a nonnegative 64-bit event ID. Duplicate, unknown and malformed selectors are rejected with HTTP 400; wrong methods with 405. A missing or inaccessible memory produces an empty `events` array, not a cross-tenant existence oracle.

The response includes `memory_id`, `events`, optional `next_after_id`, and `ordering: "event_id_not_commit_order"`. Each event carries its immutable ID, operation, status, namespace, type, current-version/supersession identifiers, authority, confidence, sensitivity, valid-time bounds, expiry and `observed_at`. No memory content or purge-audit reason is returned. Pagination uses ascending event IDs, with `next_after_id` set only when another row was observed.

All SQL executes inside the existing tenant/principal-scoped transaction as the restricted runtime database role. `memory_system_events` FORCE RLS requires both current-memory visibility and event-namespace membership/sensitivity visibility. Unauthorized event rows are not returned. Database errors are sanitized; no connection string or SQL diagnostic is returned to clients. All endpoint responses carry `Cache-Control: no-store` to prevent private history being retained by HTTP caches.

## What this does not prove

Event IDs are allocation order, **not transaction commit order**. `observed_at` is the trigger's wall-clock observation, **not commit time**. Concurrent transactions may commit in a different order, and records from before migration 000082 have only migration-time baselines. Events also do not contain memory content or every historical approval/deletion/revalidation dependency. Therefore the existing `as_of_system_time` HTTP 501 guard must remain until trustworthy complete historical belief reconstruction is implemented and tested.

Tenant offboarding still requires the existing privileged, audited purge. This endpoint cannot override a deleted memory, withdrawn membership or sensitivity policy.

## Evidence and gates

- RED: tests failed to compile before the handler/response type existed.
- GREEN: authenticated selector validation and unit tests pass.
- Restricted-role PostgreSQL integration test covers insert→update readback, keyset pagination, same-tenant nonmember denial, cross-tenant denial, membership revocation and connection-context cleanup. It runs in the existing P4 PostgreSQL CI job using `SCOS_APP_TEST_DATABASE_URL` and must pass before merging.
- Exact-head CI/P4 passed; Kilo reported no issues, Codex no findings on the initial commit, and no unresolved review threads remained. CodeRabbit/Claude were unavailable due to usage limits. PR #119 was merged with exact expected head SHA `dbeb058942d247ba65d5502f08957fd93b2bea3c`. Merged-main [CI](https://github.com/dylmarriner/scos-memory/actions/runs/40619890844) (5/5) and [P4 integration](https://github.com/dylmarriner/scos-memory/actions/runs/40619890968) (9/9) passed. Independent AMD Go suite and vet passed. Local full Python suite could not run because system Python lacks pydantic; GitHub Python CI passed.
- Pending: production deployment/recovery qualification and complete P5 historical semantics.

No production database or live service is modified by this source-only change.
