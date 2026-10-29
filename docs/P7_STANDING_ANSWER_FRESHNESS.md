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
an UPDATE or a scheduled job. A read-only HTTP endpoint is merged
into main (PR #114), but no writer/refresh generator or retrieval-orchestrator integration
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

## Read-time principal and agent clearance (migration 000089)

The security-invoker view must also enforce identity lifecycle and agent
clearance, not merely namespace membership and source freshness. Prior to
000089, a restricted `scos_app` caller with an agent principal and an
otherwise valid membership could read a cached answer above the agent's
`maximum_sensitivity`, including when the agent had no capability row.
A real PostgreSQL test reproduced the unauthorized row (RED).

Migration 000089 replaces the view with a same-tenant, enabled-principal
check and an agent capability join. It also installs a restrictive SELECT
policy on the underlying cache table for existing `scos_app` deployments;
`database/policies/000021` installs the same policy after role creation on
fresh databases. This prevents direct table SELECT from bypassing the view.
Every cited fact must be visible and below both the namespace ceiling and
the agent's maximum sensitivity; a multi-fact answer fails closed if even
one source is above clearance.
Missing agent capabilities, disabled principals and cross-tenant identities
with malformed memberships receive no cached answer. The change preserves
`security_invoker` and `security_barrier`, the legacy malformed-evidence
CASE guard, statement-time expiry checks and unresolved revalidation checks.
It neither rewrites existing cache rows nor grants additional runtime
privileges. No production migration is authorized by this source change.

The initial view-only fix still leaked through direct table SELECT (second
RED regression); the restrictive policy closes that bypass. A clean
PostgreSQL 17/pgvector installation applied 88 migrations and 22 RLS policy
files before passing both focused tests.

Real restricted-role integration exercises direct SQL table and view reads,
the memory-intelligence HTTP read endpoint and retrieval-orchestrator opt-in
standing-answer selection. Five focused repetitions per suite, two
race-enabled repetitions, full tagged PostgreSQL integration suites, the
52-package Go suite and Go vet passed on an isolated disposable database.
This closes a specific authorization defect; governed answer generation,
performance acceptance, live rollout and recovery drills remain open.

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

## HTTP read-only serving slice (merged source; not deployed)

`GET /v1/standing-answers?namespace_id=<UUID>&question_key=<text>` on
memory-intelligence reads only `public.memory_standing_answers_current` in a
transaction with authenticated tenant/principal RLS context. Question keys are
trimmed and lowercased. Unknown or repeated parameters, invalid UUIDs,
empty or oversized keys are rejected before database access. Missing,
expired, revoked, stale or inaccessible answers return HTTP 404.
Only previously generated answers are returned; there is no generation,
refresh or write API. Connection details are never included in HTTP 500
responses. Responses include `Cache-Control: no-store` so intermediaries cannot
reuse an answer after expiry or access revocation. The existing HTTP
authentication middleware remains mandatory.

Real restricted-role PostgreSQL HTTP regressions cover valid readback,
tenant isolation, membership revocation, memory revocation and clock-only
expiry. The writer, retrieval integration, latency qualification and
production rollout remain open.

PR #114 merged at `d243e4fff571b2ad848e9eff10649cb4f40db939`.
Exact-head `5b7d3029dc56c6f3ea0b74ec004578382b794d31` [CI](https://github.com/dylmarriner/scos-memory/actions/runs/40606584143)
and [P4 integration](https://github.com/dylmarriner/scos-memory/actions/runs/40606584266)
passed. The merged-main
[CI](https://github.com/dylmarriner/scos-memory/actions/runs/40607257176)
(5/5 jobs) and
[P4 integration](https://github.com/dylmarriner/scos-memory/actions/runs/40607257004)
(9/9 jobs) passed. Independently on AMD, 51 Go packages, `go vet` and
the restricted-role PostgreSQL P7 integration suite passed on merged main.
The HTTP no-store regression was reproduced RED and verified GREEN before
merge. No live migration, deployment or production recovery was performed.

## Restricted-role HTTP benchmark fixture (source candidate, 2026-10-28)

`standing_answer_http_benchmark_test.go` provides a reproducible
`BenchmarkP7StandingAnswerHTTPRead` with three measured handler paths: a fresh
evidence-backed HTTP 200, a revoked supporting fact returning HTTP 404, and
a fact that expired solely by passage of time returning HTTP 404. The
`TestP7StandingAnswerBenchmarkCasesFailClosed` integration regression checks
all three before any latency measurement. The fixture uses the actual
restricted `scos_app` PostgreSQL role, authenticated principal context and
`memory_standing_answers_current` security-invoker view.

After applying migrations and RLS policies to a **disposable** PostgreSQL
17/pgvector database and setting `DATABASE_URL` and
`SCOS_APP_TEST_DATABASE_URL`, run:

```bash
go test -tags=integration -count=1 ./services/memory-intelligence/cmd/server \
  -run '^TestP7StandingAnswerBenchmarkCasesFailClosed$'
go test -tags=integration -run '^$' \
  -bench '^BenchmarkP7StandingAnswerHTTPRead$' -benchtime=20x -benchmem \
  ./services/memory-intelligence/cmd/server
```

A local AMD Ryzen 5 2600 diagnostic run (20 operations per case) measured
3.0 ms/op fresh, 1.6 ms/op revoked and 1.5 ms/op clock-expired. A deliberate
mutation to read the raw standing-answer table reproduced HTTP 200 instead
of 404 for both revoked and clock-expired evidence, confirming the regression
would detect bypassing the view. The mutation was restored. These small local
samples are **not** a performance SLA, production benchmark or P7 acceptance.
Representative multi-tenant load, recovery and rollout qualification remain
open and require separately approved production work.

## P7 restricted agent refresh after cache invalidation (2026-10-29, PR #166 follow-up)

The `000089` migration and `000021` policy enforce an agent's maximum
sensitivity on direct standing-cache SELECT. This is essential because the
security-invoker view alone cannot prevent a restricted runtime role from
querying the base table. However, PostgreSQL's `INSERT ... ON CONFLICT DO
UPDATE` requires access to the conflicting row. When a source change marks a
previously authorized generated answer stale, the restrictive SELECT policy
hides that row, making a legitimate agent refresh fail with HTTP 500.

**TDD:** A real PostgreSQL 17 `scos_app` integration regression in
`standing_answer_stale_agent_refresh_integration_test.go` failed RED with
HTTP 500 on the original handler after a source-summary change. It passed
GREEN after introducing migration `000090_standing_answer_restricted_refresh.sql`
and fresh-install grant `policies/000022_standing_answer_restricted_refresh.sql`.
The existing restrictive SELECT policy is not relaxed. The handler delegates
the conflict update to `scos_refresh_verified_standing_answer(uuid,text)`, a
schema-qualified, owner-executed, source-derived operation. Its caller supplies
only the fact ID and normalized question key; the function derives answer,
evidence, confidence, namespace and tenant from the authenticated, eligible
source. It rechecks tenant/principal lifecycle, agent clearance, namespace
read/write membership, source validity, code revalidation, and the exact
normalized title. A source SHARE lock protects the update. The function
returns no answer text. Direct restricted-role SQL tests deny missing agent
capabilities, read-only memberships, forged question keys, insufficient
clearance and disabled principals. A stale raw SELECT remains invisible even
while authorized refresh can repair the row; a later sensitivity increase
prevents both reading and refreshing it.

The original pending-code-review HTTP 409 evidence-ineligible contract is
preserved rather than misclassified as a cache-key conflict. The full tagged
PostgreSQL memory-intelligence and retrieval integration suites passed; the
uncached Go suite passed in 52 packages, `go vet ./...` passed, and two
race-enabled focused repetitions passed. A fresh disposable PostgreSQL 17
instance installed all 112 migration/policy files, with the helper present,
the restrictive policy present, and `scos_app` EXECUTE granted. No live
migration, production deployment, recovery qualification, or §41 closure
was performed. PR-head and merged-main CI evidence must be added after merge.
