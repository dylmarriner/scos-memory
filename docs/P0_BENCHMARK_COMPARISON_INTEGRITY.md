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
