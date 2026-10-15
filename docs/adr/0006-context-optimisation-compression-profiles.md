# ADR-0006: Compression profiles are named, versioned, and capsule-backed

- **Status:** Accepted
- **Date:** 2026-07-29
- **Decision owner:** Dylan Marriner

## Context

GOVERNANCE.md §18.4–§18.5 requires named compression profiles
(`exact`/`conservative`/`balanced`/`aggressive`/`adaptive`) and a
`ContextCapsule` that makes every optimisation reversible. §35 requires a
dedicated ADR for token-optimisation policy changes.

## Decision

`services/context-optimisation` represents every optimisation run as a
`ContextCapsule` (`Profile string \`json:"compression_profile"\`` plus
`SourceObjectRef`, `ContextRange`, `TokenAccounting`,
`ValidationResults`, and `RestorationHandle`). No transform runs without
producing one of these. `InputOptimiseRequest`/`InputOptimiseResponse` and
`OutputOptimiseRequest`/`OutputOptimiseResponse` are the only entry points
— there is no code path that emits a compacted context or reply without
also emitting the capsule that can restore it (§18.5, §4.10).

High-impact or restricted tasks default to `exact` or `conservative`
(§18.4); nothing upgrades a task's default profile without an explicit,
recorded request.

## Wire contract (compiler 0.2.0, policy `context-policy-2`)

- Every token count uses one estimate, `utf8_bytes_div_4_ceil`, named in
  `token_accounting.method` and in the output and evaluate responses.
  `compiled_token_count` is the estimate of `compiled_text` as returned,
  segment headers included, so framing overhead can make
  `reduction_ratio` negative.
- `transformations` lists only what changed this input, each with the
  source byte ranges it acted on and their token counts before and after.
  `included_ranges`/`excluded_ranges` partition the stored source.
- `capsule_id` is derived from the kind, source hash, policy hash and
  compiled hash, and a manifest is stored under it beside the source; POST
  `/v1/context/restore` with `{"capsule_id": ...}` (or the full capsule,
  whose source objects must match) returns the source. Output optimisation
  stores the raw output the same way.
- `validation.checks` are measured: retention of hints, exact values and
  pinned references; `source_hash_recorded` and `restoration_verified` read
  the source and capsule back from the store.
- `code_symbols` and `memory_references` must occur in the input; segments
  containing them are kept verbatim and checked. `model`,
  `session_context` and `request_intent` had no implementation and are
  rejected as unknown fields, as are unknown profiles and the `diff`
  output format.
- Readiness writes to the configured source store, not the temp directory.

## Consequences

- Every compacted artifact is restorable by construction, not by
  convention — the capsule is part of the response type, not an optional
  side record.
- Changing what `balanced` or `aggressive` are allowed to do is a policy
  change to the transform pipeline, not a silent behavior change, because
  the profile name is recorded in the capsule for every historical run.

## Compatibility

`ContextCapsule` fields are additive. A profile's *behavior* can change
between policy versions, but a capsule always states which
`compiler_version`/`policy_version` produced it, so old capsules remain
interpretable even after profile behavior changes.

## Migration

None at the schema level — capsules are request/response payloads, not
stored rows requiring migration; persistence of capsule references (where
retained) follows the same append-only rules as other authoritative
records (§4.1).

## Rollback

Reverting a profile's transform pipeline to a prior `compiler_version` is
the rollback mechanism; because capsules pin the version that produced
them, a rollback does not retroactively invalidate capsules created under
the newer version.

## Index rebuild

Not applicable — capsules and their restoration handles reference
authoritative source objects directly; there is no derived index to
rebuild for this layer.

## Verification

`ValidationResults` on every capsule proves the §18.2 non-droppable
classes (governing policy, current instruction, acceptance criteria, exact
identifiers, evidence) survived compilation before the response is
returned. Failure injection's `compression_*` category
(`lost_mandatory_instructions_during_compression`,
`altered_exact_values`, `broken_restoration_handles`,
`tokenizer_mismatch`) is the standing regression suite. The §18.10
superiority gate (≥50% median token reduction, 100% restoration success,
≥99.5% mandatory-instruction preservation) is the acceptance bar before any
profile may be called superior to the unoptimised baseline.
