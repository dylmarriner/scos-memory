# P7: deterministic single-fact standing-answer refresh

**Status:** source implementation and disposable-database acceptance; full P7
answer generation, retrieval integration, and production rollout remain open.

`POST /v1/standing-answers/refresh` takes only `{"fact_id":"<uuid>"}`.
The authenticated tenant/principal must be able to read the source memory and
write candidates in its namespace. The handler derives the normalized exact
`question_key` from that memory's **title**, the `answer` from its already
persisted **canonical_summary**, and confidence from the source. Callers cannot
supply arbitrary answer prose, question keys, namespaces, or evidence arrays.
This is an exact-title materialization, **not** an LLM-generated answer to a
new natural-language question. It is useful only when a memory's title is an
appropriate exact lookup key.

The source memory is selected under transaction-local FORCE RLS and a row
SHARE lock. It must be active, not superseded, within valid/expiry bounds and
under the namespace sensitivity ceiling. The existing dependency trigger
checks supporting evidence, version, and unresolved P7 code revalidation.
Its specific SQLSTATE 23514 evidence-ineligible rejection is mapped to HTTP
409, not a misleading HTTP 500; unrelated database errors still fail closed.
Before committing, the writer checks the security-invoker current-evidence
view in the same transaction. It returns no answer on failed evidence
validation. Refresh is idempotent for the same canonical UUID and refuses
to overwrite a curated answer or a fresh answer with a different source fact.
A stale, previously machine-generated exact-title entry may be refreshed from a
new current same-title source; even stale curated entries remain protected.
`Cache-Control: no-store` prevents stale intermediary reuse.

Migration `000086_standing_answer_source_text_invalidation.sql` extends the
existing transactional dependency invalidation trigger to changes in
`memory_items.title` and `memory_items.canonical_summary`, not just version,
status and expiry. This closes a direct-SQL source-text change that previously
left stale cached prose readable indefinitely. Because previous untracked
text changes cannot be reconstructed, the migration invalidates every
previously fresh cache row. It takes SHARE ROW EXCLUSIVE locks on both tables
while installing the trigger and invalidating the rows. **Production rollout
requires an explicit operator-approved migration window, lock/duration
qualification, rollback/recovery plan and controlled refresh**. No live
migration is authorized by merging this source.

## Verification and limits

Real PostgreSQL 17/pgvector restricted-role HTTP tests cover source-derived
answers, exact evidence IDs, idempotent refresh, canonical uppercase UUIDs,
foreign-tenant denial, malformed/forged JSON, read-only membership denial,
revoked evidence, direct-SQL summary/title changes, and collisions with
curated/different-source answers. The source-text mutation test returned HTTP
200 for a stale answer before migration `000086` (RED), and HTTP 404 after
migration (GREEN). A separate uppercase-UUID regression failed with HTTP 409
before canonicalization and passed after. The handler's initial test failed
to compile before implementation, then passed. Two PR review regressions failed RED on the original candidate: an invalidated
machine-generated same-title cache could not switch to its current successor
(HTTP 409 instead of 200), and unresolved P7 code review returned HTTP 500
instead of an evidence-ineligible 409. Both passed GREEN five consecutive
focused PostgreSQL runs; a stale curated entry remains unreplaceable and a
reviewer still_valid decision restores refresh eligibility. The full
memory-intelligence/retrieval-orchestrator integration suites passed on a
separate disposable PostgreSQL 17/pgvector database after the correction.
Uncached Go suite, vet and race verification are tracked separately in the PR.

This does **not** close the full governed-writer requirement: existing
`memory_standing_answers` direct-SQL INSERT/UPDATE permissions for the
runtime role can still write cache rows outside this new HTTP path. A future
trusted generation pipeline must close or explicitly govern that bypass,
verify question/answer relevance beyond exact-title matching, integrate
retrieval, qualify latency and freshness across tenants, and demonstrate
production deployment/recovery. Governance §41 and P0–P8 remain PARTIAL.
