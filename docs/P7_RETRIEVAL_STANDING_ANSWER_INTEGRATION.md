# P7 opt-in standing-answer retrieval integration

**Status:** PR #126 merged at `7b38ee57a4fd9e5a58ea2316b0bdb09e865004a7` with passing merged-main CI/P4; follow-up audit/CI correction merged in PR #127 at `1a91f216f9a66124575e87c04297158481f8ff7f` after CodeRabbit policy-path review remediation, passing final-head and merged-main CI/P4. Not production deployed. The canonical P7 acceptance gate remains open.

`POST /v1/retrieve` accepts `include_standing_answers: true` as an explicit
opt-in to the already materialized, exact-title standing-answer cache. A
successful response may include a separate `standing_answers` array containing
the cache row UUID, namespace UUID, normalized question key, cached answer,
complete supporting memory UUIDs, confidence, and refresh timestamp. This
array is **not** mixed into ordinary ranked memory sections or represented as
a new model-generated answer.

The MCP gateway's `scos_retrieve` tool schema exposes the same optional
boolean and forwards it with the gateway-verified identity; it does not
fabricate a tenant or bypass the orchestrator's validation.

The retrieval orchestrator queries only
`public.memory_standing_answers_current`, the security-invoker,
security-barrier view introduced by migration 000085. The query runs with
transaction-local authenticated tenant/principal context and FORCE RLS. It
requires an exact trimmed/lowercased key match, an authorized namespace and
visibility of every supporting fact. An additional all-evidence predicate
enforces the caller's narrower `sensitivity_max` limit, not just the
namespace ceiling. No answer is returned if the view excludes revoked,
expired, superseded, hidden or unresolved code-revalidation evidence.

Cache lookup is unavailable for `as_of_valid_time`, `as_of_commit`,
`snapshot_id` or `memory_classes` selectors; these combinations return
HTTP 400 rather than misrepresenting current cached text as historical or
class-filtered evidence. `as_of_system_time` retains its existing HTTP 501
guard. A cache lookup key must be nonempty UTF-8, without NUL, and at most
1024 bytes. No cache lookup runs unless explicitly requested.

Cache output shares the request's token budget with selected memory
sections. Answers are either returned in full with their evidence or
omitted, never truncated into misleading fragments. The response has
`Cache-Control: no-store`. Cache database errors fail closed with HTTP 503.
For every explicit cache lookup, including a cache miss, failure to persist
the retrieval audit fails closed with HTTP 503. An earlier implementation
returned HTTP 200 with an empty audit ID when the cache was empty; this
was reproduced and corrected with a restricted-role PostgreSQL HTTP regression. `retrieval_runs.filters` records the selected cache UUIDs,
namespace UUIDs, supporting memory UUIDs, SHA-256 of each exact answer and
refresh timestamp; `candidate_counts` and `token_estimate` account for
returned cache answers.

## Verification

TDD first failed at compilation because the exact-title normalization
function did not exist. After implementation, unit tests cover incompatible
temporal/class selectors, key validation, and no partial-answer budgeting.
A disposable PostgreSQL 17/pgvector database with migrations 000001–000087
and all RLS policies exercises the full retrieval HTTP handler under the
non-superuser, non-BYPASSRLS `scos_app` role. It checks exact answer/evidence
readback, opt-in requirement, durable audit, a narrower sensitivity ceiling,
mixed-sensitivity evidence, token exhaustion, membership revocation and
source revocation. No production database is modified.

## Audit-failure and CI-coverage correction (2026-10-28)

A cache miss with a syntactically valid but nonexistent `session_id` caused
`retrieval_runs` to reject its audit INSERT on a foreign-key constraint. The
P7 handler previously returned HTTP 200 with `retrieval_run_id: ""` because
its fail-closed check only applied when `standing_answers` was nonempty. A
new full-handler test reproduced the defect (RED) against a disposable
restricted-role PostgreSQL 17/pgvector database; the fix now returns HTTP
503 for any opt-in lookup whose audit cannot be committed (GREEN). The test
also checks that no unaudited retrieval payload is returned. Ordinary
non-cache retrieval retains its existing best-effort audit behavior.

The P4 PostgreSQL workflow previously invoked `go test` without the
`integration` build tag for the retrieval package, excluding this P7 HTTP
test. A Python workflow-contract test failed RED and now passes after the
workflow was changed to run `go test -tags=integration -count=1` for the
retrieval package. CodeRabbit additionally found that policy-only PRs would
not trigger this PostgreSQL job. A second RED/GREEN workflow-contract test
now requires `database/policies/**` in both pull-request and push filters. Local verification passed the complete tagged retrieval
suite and two focused race-enabled repetitions on a disposable database
with all 86 migrations and 21 RLS policy files. This is not production
rollout evidence or general answer generation.

This is **not** general answer generation, contradiction adjudication,
historical belief reconstruction, proof of commit-order history, full
P7 acceptance or production rollout/recovery qualification. Existing
standing-answer writer and operator approval boundaries remain separate.
A transaction ending before the HTTP response cannot guarantee that no
revocation commits in the intervening network interval; the response must
not be treated as a durable authorization grant. Further concurrency,
representative latency, and fleet-wide integration evidence remain open.
