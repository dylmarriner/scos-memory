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
