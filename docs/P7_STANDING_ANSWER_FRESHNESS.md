# P7 standing-answer evidence freshness (migration 000084)

This is a **database-level invalidation slice**, not a complete cache service,
an automatic answer generator, or production deployment.

## Provenance and write contract

The migration adds `stale_at` and `stale_reason` to
`memory_standing_answers`, plus typed
`memory_standing_answer_dependencies` with memory UUIDs and the supporting
version observed at refresh. The dependencies are trigger-maintained and not
directly readable or mutable by the restricted runtime role.

A standing answer may be inserted/refreshed as fresh only when its JSON
`evidence_fact_ids` is a nonempty UUID-string array of real active facts
in the same tenant and namespace. Each fact must be effective and unexpired,
below the namespace sensitivity ceiling, not superseded, and free of unresolved
P7 code-revalidation events. A reviewed `still_valid` decision permits the
evidence to be used again; superseded, invalidated and unresolved decisions do
not silently restore freshness.

Existing standing answers are conservatively marked
`unverified_legacy_evidence` during migration. The migration does **not**
pretend their historical free-text evidence lists are authoritative.

## Automatic stale transitions

Each fresh cache registration takes a `FOR SHARE` lock on its supporting
memory rows until commit. This serializes with concurrent correction/version
updates; without it, the correction trigger could run before an uncommitted
cache dependency is visible and leave stale evidence incorrectly fresh.
A real two-transaction regression reproduced this race before the lock and
passed after the fix.

In the same PostgreSQL transaction as a fact status, version, supersession,
effective-date, expiry, sensitivity or namespace update, only standing
answers linked to that fact are marked stale. Deleting a fact marks its
dependent answers stale before evidence FK cascade. Unrelated metadata writes
do not mark them stale.

A newly inserted P7 revalidation event requiring review also marks the
answers dependent on its fact stale, without editing memory truth or making
any human review decision. An attempted refresh using unresolved P7 evidence
fails. The cache remains stale until an authorized, evidence-valid refresh.

The mutation triggers alone cannot detect when the wall clock passes a
previously future `valid_until` or `expires_at`. Migration `000085`
provides a read-time projection that checks those bounds without relying on
an UPDATE or a scheduled job. A new read-only HTTP endpoint is a source
candidate, but no writer/refresh generator or retrieval-orchestrator integration
is implemented, so this does not close P7 acceptance.

## Read-time projection (migration 000085)

`memory_standing_answers_current` is a PostgreSQL `security_invoker`,
`security_barrier` view over only non-stale answers whose complete evidence
list still resolves to active, effective, unexpired, non-superseded, visible
facts within the same tenant and namespace. The view uses
`statement_timestamp()`, not transaction-start `now()`, so a long-lived
transaction cannot serve evidence that expired after it began. It also rechecks unresolved
code-revalidation events. A missing, revoked or hidden fact makes the answer
unreadable. It applies the caller's forced row-level policies rather than
running as the migration owner; ordinary `scos_app` receives SELECT only.

The view never writes cache state. It deliberately does not change `stale_at`
when time passes, so it is safe for historical audit and future read services.
A stale legacy row with malformed free-text evidence is excluded without
parsing that text. Tests use a real PostgreSQL clock transition and a
restricted-role membership revocation. The service integration, answer
generation, retrieval-orchestrator integration, and read-path benchmarking
remain separate acceptance gates.

## Isolation and recovery

- Migration `000084` is append-only and does not modify any production data
  without an approved deployment.
- The three `SECURITY DEFINER` triggers use `public.`-qualified application
  tables and place `pg_temp` last in their search path. Real restricted-role
  testing reproduced a temporary-table shadowing attack before the fix:
  a fake dependency table caused zero real dependencies to be recorded.
  The test passes after schema qualification.
- The migration owner has an owner-only RLS UPDATE policy to support
  `SELECT ... FOR SHARE` on evidence rows under forced RLS. The ordinary
  application role does not receive that exception.
- The dependency table uses forced RLS and an owner-only maintenance policy;
  `scos_app` direct table privileges are revoked both during upgrades and
  after the fresh-install blanket grant in `database/policies/000011_app_role.sql`.
- Privileged trigger operations occur in the same transaction as the memory
  state or P7 event. Failed cache evidence validation rolls back the write.
- The existing standing-answer namespace-membership policy is retained.
- Automatic invalidation does not delete audit evidence or rewrite approved
  memory facts.

## Verification

The new real PostgreSQL integration tests under the restricted app role
cover exact dependency registration; unrelated metadata changes; status
supersession; fact deletion; attempted stale-answer reactivation; approved
reviewer `still_valid` recovery; changed-code `needs_review` invalidation;
cross-tenant evidence rejection; direct dependency-table denial; and a
concurrent memory correction/cache-refresh regression; and a restricted-role
  temporary-table shadowing attack regression.

Run with a disposable migrated PostgreSQL 17/pgvector database with all
policies and both `DATABASE_URL` (migration owner) and
`SCOS_APP_TEST_DATABASE_URL` (non-superuser, non-BYPASSRLS runtime role):

```bash
go test -tags=integration -count=1 ./services/memory-intelligence/cmd/server \
  -run '^TestP7StandingAnswer'
```

Migration `000084` merged in PR #111; exact merged-main CI and P4 workflows
passed on commit `6030318ac61e367844fec061688e773e096b71de`.
Migration `000085` merged in PR #112 at
`50f2252d242edb8cb795fe6f274322887610f89c`. Its exact-head
[CI](https://github.com/dylmarriner/scos-memory/actions/runs/40603566260)
and [P4 integration](https://github.com/dylmarriner/scos-memory/actions/runs/40603566254)
passed, as did merged-main
[CI](https://github.com/dylmarriner/scos-memory/actions/runs/40604314213)
(5/5 jobs) and
[P4 integration](https://github.com/dylmarriner/scos-memory/actions/runs/40604314371)
(9/9 jobs). The full merged-main 51-package Go suite, vet and tagged
memory-intelligence PostgreSQL integration suite passed independently on AMD.
Kilo review passed; the Codex statement-time finding was reproduced RED,
fixed and regression-tested GREEN before merge. No production database has
been changed; live rollout and recovery qualification require separate approval.

## HTTP read-only serving slice (source candidate; not deployed)

`GET /v1/standing-answers?namespace_id=<UUID>&question_key=<text>` on
memory-intelligence reads only `public.memory_standing_answers_current` in a
transaction with authenticated tenant/principal RLS context. Question keys are
trimmed and lowercased. Unknown or repeated parameters, invalid UUIDs,
empty or oversized keys are rejected before database access. Missing,
expired, revoked, stale or inaccessible answers return HTTP 404.
Only previously generated answers are returned; there is no generation,
refresh or write API. Connection details are never included in HTTP 500
responses. The existing HTTP authentication middleware remains mandatory.

Real restricted-role PostgreSQL HTTP regressions cover valid readback,
tenant isolation, membership revocation, memory revocation and clock-only
expiry. The writer, retrieval integration, latency qualification and
production rollout remain open.
