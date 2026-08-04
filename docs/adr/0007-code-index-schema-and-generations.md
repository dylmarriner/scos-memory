# ADR-0007: Code indexes are atomic, branch-scoped generations over authoritative source

- **Status:** Accepted
- **Date:** 2026-07-29
- **Decision owner:** Dylan Marriner

## Context

GOVERNANCE.md §19.3–§19.4 requires stable repository identity, atomic
incremental index generations, and zero partially-committed-generation
reads. §35 requires a dedicated ADR for code-index schema changes.

## Decision

`services/code-intelligence` models a `Repository` (with
`IndexGeneration int`, canonical path, remote identity, branch/worktree,
commit ID, language set, and hashes) and a separate `IndexGeneration`
record per rebuild. A `Chunk` is the unit of retrieval: it carries
`symbol_id`, `fully_qualified_name`, `kind`, `signature`, `path`, `range`,
`content_hash`, `commit_id` and relation fields, not an arbitrary
fixed-size window (§19.6).

Queries (`CodeQuery` → `CodeQueryResult`) always resolve against one
committed `IndexGeneration`; a rebuild in progress does not affect what a
concurrent query sees, and no query result may straddle two generations.
`ChangeImpactRequest` → `ChangeImpact` is a distinct read path used before
a proposed change, not a byproduct of the index-build path.

## Consequences

- Symbol-aware chunking means callers get complete definitions/signatures
  instead of arbitrary character windows, at the cost of requiring a
  working language adapter for each supported language tier.
- Index rebuilds are safe to run concurrently with queries because
  generation is the atomicity boundary, not the individual file/symbol
  row.

## Compatibility

`Repository`, `Chunk` and `IndexGeneration` fields are additive. A new
language adapter only needs to satisfy the common contract (`detect`,
`parse`, `extract_symbols`, `extract_references`, `extract_relations`,
`extract_diagnostics`, `slice_context`, `validate_change` — §19.5); it does
not require a schema change to the generation/chunk model.

## Migration

A repository's index is not migrated in place — a schema or parser change
that affects extraction produces a new `IndexGeneration` via full rebuild,
never a row-level backfill of the old generation.

## Rollback

Rollback is: continue serving the last complete generation
(`index_generation - 1`) while the new generation's build is discarded.
Because generations are immutable once committed, "rollback" here is
selection, not reconstruction.

## Index rebuild

This is the layer's core operation, not an exceptional path: incremental
indexing reparses only affected files and dependent symbols using content
hashes to skip redundant work (§19.4); full rebuild from source is always
available and is the authority-recovery path when a generation is judged
untrustworthy (parser bug, corruption).

## Verification

Failure injection `code_intel_*` category (stale generation, partial
index, parser crash, repository rename, cross-branch leakage, malformed
syntax) is the standing regression suite. The §19.13 superiority gate
(≥98% exact lookup, ≥95% symbol-definition recall, zero cross-project
leakage, atomic generation consistency) is the acceptance bar, reported by
language/scale/tier rather than a single averaged number.
