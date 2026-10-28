# P7: deterministic multi-fact evidence draft (source candidate)

This endpoint is **not general answer generation**, not a standing-answer
cache writer, and not a published/approved answer. It provides a reviewable
draft by combining **verbatim canonical summaries** of 2–8 current governed
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
