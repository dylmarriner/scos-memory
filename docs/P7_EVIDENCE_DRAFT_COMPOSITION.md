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
encoding and domain prefix `scos:p7:source:v2\\0`. Old v1 fingerprints must
be treated as stale by future approval implementations, not silently upgraded.
The SHA-256 value is still a drift detector, not an authorization token or
signature; every approval must re-check source state and access rights.
