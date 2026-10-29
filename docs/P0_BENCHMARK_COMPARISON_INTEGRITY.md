# P0 benchmark comparison: metric direction and measurement coverage

The benchmark runner compares **the same case** against a previously recorded
baseline. Previously, the comparator classified *every* higher numeric value
as an improvement, including the undesirable `stale_hit_rate` and
`duplicate_rate`. A retrieval change that exposed more stale evidence or
more duplicate records could therefore be mislabeled as a quality gain.

The corrected comparison explicitly treats `stale_hit_rate`,
`duplicate_rate`, and `answer_regression` as **lower is better**.
Recall, precision, reciprocal rank, nDCG, authority/conflict accuracy,
citation accuracy, promotion precision/recall and token efficiency retain
**higher is better** semantics.

A second issue was measurement disappearance. If a candidate stopped reporting
citations or usage, `compare_baseline` silently omitted that metric from its
comparison. It now reports **coverage_lost** or **coverage_gained** per
paired case. A missing observation is not scored as zero, nor mistaken for
an improvement or regression. A candidate's report must be reviewed for
coverage losses as well as directional quality regressions.

## Test-driven evidence

- Four new regression tests first failed (two inverted quality directions
  and two silently missing optional metrics) and then passed after repair.
- The P0 benchmark harness tests and P4 deterministic offline gate pass.
- The P4 lexical baseline was **requalified** because the harness source
  hash changed; its fixture, all seven rankings, all quality scores and all
  forbidden-evidence counts are identical to the previous baseline.
- The updated baseline records the new harness source hash and Git base SHA.

This is **score interpretation integrity**, not a new model-quality result.
It does not measure actual BGE-M3/BGE-Code backend accuracy, real latency,
full conflict/supersession retrieval, or live production effectiveness.

## Shared Python benchmark package parity (2026-10-29)

The standalone `packages/benchmark-harness/python/scos_benchmark/metrics.py`
implementation still diverged from the Go and `benchmarks/harness.py`
scorers. A repeated relevant result inflated its precision denominator by
collapsing repeated ranking slots, and nDCG credited the same source twice.
A ranking `[a, a, b]` against `{a, b}` scored **1.0 precision** rather
than **2/3**, and **1.30657 nDCG**, an impossible score above 1. Repeated
truth labels also reduced recall (0.5 instead of 1.0).

Four Python unit regression cases were added. Three failed against the original
implementation (RED); all passed after using unique truth sets, retaining
all returned ranking slots for precision, and counting each relevant source
once for nDCG (GREEN). The fourth checks nonpositive K. The repository CI
Python job now runs the standalone package's tests and the canonical harness
ranking-integrity tests so the two implementations cannot silently drift.
A separate Go regression also failed RED: `ComputeNDCGAtK` returned 1.0
for a single relevant hit when two distinct relevant IDs were expected and
`k=5`, because it shortened the ideal ranking to the number of returned
results. The corrected denominator uses the requested K and the unique truth
count, matching the canonical Python harness (GREEN: 0.6131471928).

This corrects **metric integrity only**. It is not a deployed benchmark, a
representative quality/latency/cost measurement, or P0 completion evidence.
