# P7: deterministic multi-fact evidence draft (source candidate)

This endpoint is **not general answer generation**, not a standing-answer
cache writer, and not a published/approved answer. It provides a reviewable
draft by combining **quoted canonical summaries** of 2–8 current governed
facts. It makes no new factual inference.

## Contract

`POST /v1/standing-answers/compose` with JSON:

```json
{
  "namespace_id": "11111111-1111-4111-8111-111111111111",
  "fact_ids": [
    "22222222-2222-4222-8222-222222222222",
    "33333333-3333-4333-8333-333333333333"
  ]
}
```

Requires an authenticated tenant/principal. The request is size-bounded,
rejects unknown or duplicate JSON keys, duplicate/zero/invalid UUIDs and
more than eight sources. UUIDs are canonicalized and sorted so response order
does not depend on the caller's input order.

Each source must be visible through the caller's transaction-local RLS,
within the exact tenant and namespace, readable under current membership,
active, not superseded, unexpired, within the namespace sensitivity ceiling
and without an unresolved P7 code-revalidation event. A reviewed
`still_valid` event is eligible. If **any** source is missing or inaccessible,
the whole request returns HTTP 404, not a partial draft.

The response includes `status: draft_requires_review`, `draft_text`,
individual `evidence` entries with source UUIDs, verbatim summaries and
source confidence, and a conservative minimum source confidence. Invalid or
oversized summaries fail closed without truncation. The endpoint never writes
`memory_standing_answers`, promotes a memory, calls a model or changes
governance state. `Cache-Control: no-store` is always set.

`draft_text` quotes each summary using Go string-literal escaping, so embedded
newlines, carriage returns and terminal control sequences from an authored
fact cannot forge additional `Source fact <uuid>:` provenance headers or
alter reviewer-facing terminal output. The `evidence[].summary` field still
contains the exact unmodified canonical source text for independent review.
This is a presentation-integrity boundary, not sanitization of the source or
approval of its factual claims.

## Evidence and limitations

The implementation was first tested RED with the missing handler, then GREEN
against an isolated PostgreSQL 17/pgvector instance with the full 86-migration
and restricted-role policy stack. Tests cover authenticated HTTP, malformed
requests, order stability, source provenance, no cache write, foreign tenant,
revocation, expiry, membership revocation and unresolved/reviewed code events.

This is a bounded **source-assembly primitive**, not completion of P7 general
governed answer generation. Human review, provenance-rich natural-language
synthesis, persistence/approval workflow, representative multi-tenant
performance, deployment and recovery evidence remain open.

## 2026-10-29 provenance-spoofing regression (source candidate)

A real restricted-role PostgreSQL HTTP regression inserted a source summary
containing a forged `Source fact` header, CR and ANSI cursor controls. Before
the fix, the review draft included the forged header and raw controls (RED).
Quoting untrusted source text in `draft_text` prevents the spoof while leaving
`evidence[].summary` byte-for-byte intact (GREEN, five focused repetitions,
two race-enabled repetitions). The full memory-intelligence and retrieval
PostgreSQL integration suites passed against a disposable PostgreSQL 17/pgvector
instance with 86 SQL migrations and 21 policies. PR #141 was merged into `7daa74e2c36deeee5bf4f2cebba5704f8d48e0ab`
after CI and review, with merged-main CI/P4 passing 5/5 and 9/9 jobs.
This is verified merged source, not production evidence.

## 2026-10-29 disabled and cross-tenant principal guard (source candidate)

A restricted-role PostgreSQL HTTP regression exposed two membership edge
cases: a principal with `disabled_at` set could still compose a draft using
its retained membership, and a principal from a different tenant could
compose evidence if an erroneous cross-tenant namespace membership existed
and the identity pair named the namespace's tenant. Both cases returned
HTTP 200 before the fix (RED). The composition query now joins the
`principals` table and requires a matching tenant and an enabled principal,
in addition to the existing namespace membership, RLS, and source checks.
Both cases return HTTP 404 (no partial evidence) after the fix (GREEN),
including five focused restricted-role test repetitions, two race-enabled
repetitions, the full memory-intelligence and retrieval-orchestrator
integration-tagged PostgreSQL suites, 52 Go packages, `go vet ./...`, and
`git diff --check`. All 86 migrations and 21 policies were applied to a
disposable PostgreSQL 17/pgvector instance. The governance §41 definition-of-
done checker still returns FAIL (PARTIAL). This change is
limited to the P7 composition endpoint and does not replace central identity
revocation, fix cross-tenant memberships elsewhere, or qualify production.


## 2026-10-29 source fingerprint and version pinning (source candidate)

A draft's source UUID alone cannot prove which version of the canonical summary
a reviewer saw. Each `evidence[]` entry now includes the optional
`version_id` from `memory_items.current_version_id` and a lowercase
64-character `source_fingerprint` SHA-256. The versioned digest is computed
over the exact source fact UUID, current version UUID (empty for NULL), summary
bytes and Go round-trip decimal confidence, separated by NUL and prefixed
`scos:p7:source:v1\0`. This binds the fingerprint to the evidence presented,
including unversioned facts, without changing the draft's stable ordering or
creating an approved answer. A future review/publish operation must re-read
current evidence under authorization and compare the fingerprint before
approving anything; the digest alone is **not** a signature, proof of factual
truth, or a complete approval protocol.

TDD first failed compilation because the fingerprint and version fields were
missing (RED), then passed (GREEN). A fixed SHA-256 test vector guards the
encoding contract. Real PostgreSQL restricted-role HTTP regressions verify
that changing the canonical summary or current version changes only the
affected source's digest, while other source digests remain stable and no
standing-answer cache entry is written. On a disposable PostgreSQL 17/pgvector
instance, all 86 migrations and 21 policies applied; the focused integration
suite passed five repetitions, full memory-intelligence and retrieval-orchestrator
integration suites passed, the 52-package Go suite passed, focused race tests
passed twice, and Go vet passed. This remains a review-drift detection
primitive, not general governed answer generation or P7 acceptance.

## P7 source fingerprint security-context binding (2026-10-29)

The v1 digest pinned fact/version/summary/confidence but not the security
context in which the draft was reviewed. Moving otherwise unchanged evidence
to a different namespace could therefore leave its digest identical, allowing
a future approval workflow to mistake a previous review for a review in the
new context. The v2 digest now includes the authenticated tenant UUID and
requested canonical namespace UUID before the fact fields, with NUL-separated
encoding and domain prefix `scos:p7:source:v2\0`. Old v1 fingerprints must
be treated as stale by future approval implementations, not silently upgraded.
The SHA-256 value is still a drift detector, not an authorization token or
signature; every approval must re-check source state and access rights.

## Agent maximum-sensitivity enforcement (2026-10-29; source candidate)

Namespace membership and its sensitivity ceiling do not override an agent's
independent `agents.maximum_sensitivity` capability. The composition query
now joins the agent capability row for `principals.kind='agent'` and rejects
sources above that capability; a missing capability row fails closed. Human
principals remain governed by their existing namespace permissions and
sensitivity ceiling. Mixed-sensitivity drafts are rejected as a whole rather
than silently omitting the restricted source.

A restricted `scos_app` PostgreSQL HTTP integration test reproduced HTTP 200
for an `internal`-limited agent composing an `internal` and `confidential`
source in a `confidential` namespace (RED). The same test verifies HTTP 404
for the over-limit draft, HTTP 200 after an explicit agent capability upgrade,
and HTTP 404 after removing the capability row (GREEN). Five repetitions of
the focused P7 composition suite and two race-enabled repetitions of the
agent-clearance regression passed on a disposable PostgreSQL 17/pgvector
instance with 86 migrations and 21 policies. This protects the P7 draft
composition path only; it does not prove that all other SCOS read paths
consistently enforce agent clearance or complete governed approval.

## 2026-10-29 review-time expected fingerprint (source candidate)

A caller that has independently reviewed a previous draft can resubmit the
same `namespace_id` and `fact_ids` with
`"expected_draft_fingerprint": "<64 lowercase hex SHA-256 characters>"`.
The server performs the ordinary current restricted-role/RLS read and
recomputes the entire review manifest. Matching digests return HTTP 200 and
the normal `draft_requires_review` response. Changed evidence or display text
returns HTTP 409 with no draft or source payload. An inaccessible or revoked
source returns HTTP 404 before digest comparison, and invalid/empty/uppercase
digests return HTTP 400. The field is optional for initial composition, but
when present must be valid; `null` is not an opt-out. No state is persisted.

The precondition helps reject stale review evidence. It does not authenticate
a reviewer, approve a fact, publish an answer, prevent future changes after
this read, or establish atomic compare-and-publish semantics. A future
approval path must repeat authorization and manifest validation in the same
transaction as its governed write.

TDD: unsupported well-formed field and empty-value bypass failed RED, then
passed GREEN. Restricted-role PostgreSQL integration tested unchanged and
changed evidence, cross-tenant denial, no stale-draft disclosure and no
standing-answer writes. Five focused repetitions, two race-enabled
repetitions, the full memory-intelligence and retrieval-orchestrator
integration suites, 52-package Go tests and Go vet passed on a disposable
PostgreSQL 17/pgvector instance with 86 migrations and 21 policies. No
production deployment, migration or cache publication was performed.

## P7 sensitivity-aware source review fingerprints (2026-10-29; PR candidate)

The source fingerprint v2 covered tenant/namespace, fact UUID, current version,
summary and confidence, but omitted the source sensitivity classification.
Changing a still-readable source from `public` to `internal` therefore left
`expected_draft_fingerprint` valid even though the security context of the
reviewed evidence had changed. The regression demonstrated HTTP 200 with an
outdated pinned digest before correction.

`evidence[].sensitivity` now exposes the current classification and the
versioned `scos:p7:source:v3\0` digest binds it before the source summary.
The whole-draft manifest transitively binds the classification as well.
Old v2 fingerprints must fail the new precondition, not be silently accepted.
A restricted-role PostgreSQL integration test confirms unchanged pinned
drafts still pass, classification changes within the same readable namespace
return HTTP 409 without evidence leakage, and a fresh draft exposes both
current classifications. A separate fixed SHA-256 vector and unit regression
cover digest determinism and classification sensitivity. This does not
implement approval, publishing or transactionally atomic review-and-write.
