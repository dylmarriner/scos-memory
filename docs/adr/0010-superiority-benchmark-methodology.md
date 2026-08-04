# ADR-0010: Superiority claims require frozen, comparative, reproducible benchmarks

- **Status:** Accepted
- **Date:** 2026-07-29
- **Decision owner:** Dylan Marriner

## Context

GOVERNANCE.md §4.13 (measured superiority) and §33 (quality evaluation)
prohibit claiming any subsystem superior without reproducible comparative
evidence against an unoptimised baseline, the prior production version,
and the strongest lawfully-evaluable competing implementation. Four
subsystems carry their own numeric superiority gates: context optimisation
(§18.10), code intelligence (§19.13), execution proxy (§20.11), and the
autonomous control plane (§21.18). §35 requires a dedicated ADR for
superiority-benchmark changes.

## Decision

`packages/benchmark-harness` provides the shared metric primitives
(`ComputeRecallAtK`, `ComputePrecisionAtK`, `ComputeMRR`,
`ComputeNDCGAtK`) used by every comparative run so that "recall@k" means
the same computation across subsystems. `benchmarks/harness.py` and the
frozen retrieval corpus are the reference implementation of the process
this ADR requires:

1. Freeze the benchmark corpus before the comparative run that will be
   cited (§33: "Benchmark corpora SHALL be frozen before final comparative
   runs").
2. Run against: an unoptimised baseline, the prior production version, and
   the strongest tested competing implementation.
3. Report confidence intervals, dataset composition, exclusions, hardware,
   model, version, cost, latency, and the task-success criterion used —
   not just the headline metric.
4. A subsystem may be labelled superior only when its section's specific
   gate (§18.10/§19.13/§20.11/§21.18) is cleared in full — winning one
   favourable metric while degrading correctness is failure, not partial
   success (§4.13).

Cherry-picked demonstrations are explicitly not evidence (§33).

## Consequences

- No subsystem can claim superiority from a single favourable internal
  run; the corpus-freeze requirement forces the comparison to be decided
  before the numbers are known.
- Adding a new competing baseline to compare against is a benchmark change
  covered by this ADR, not a silent methodology drift.

## Compatibility

`packages/benchmark-harness` metric functions are pure and additive — new
metrics can be added without changing what an already-published
`Recall@K`/`MRR`/`nDCG` number means for a prior benchmark generation.

## Migration

None at the schema level. A new benchmark generation is a new frozen
corpus plus a new run, not a rewrite of a previous generation's recorded
results.

## Rollback

If a benchmark run is later found to be methodologically invalid (leaked
test data, incorrect baseline configuration), the fix is publishing a
correction alongside the original — the original result is marked invalid
in place, never silently deleted, consistent with §4.6 (correction, not
silent mutation) applied to benchmark evidence.

## Index rebuild

Not applicable.

## Verification

The frozen retrieval/acceptance benchmark has grown across several
generations under this process, from an initial nine-case corpus up to the
current 24/19/20/24-case corpus (context optimisation / code intelligence /
execution proxy / control-plane routing) recorded in
`benchmarks/acceptance-results.json` and independently reproduced
byte-for-byte by re-running `benchmarks/run_acceptance.py` (see
`docs/governance-audit-current.md`, which supersedes the earlier
`docs/governance-implementation-audit.md` and `docs/governance-audit-full.md`
audit documents that tracked this corpus's growth over time). This is a
real step up in scale and diversity from the original nine-case generation,
but it is still a modest, hand-authored corpus rather than the large,
statistically powered population §33 and the section-specific superiority
gates (§18.10/§19.13/§20.11/§21.18) ultimately describe. Continuing to grow
this corpus, not re-labelling the current one as sufficient, remains the
correct next step.
