# P4 hybrid-retrieval smoke corpus

`retrieval_fixture.json` is the first frozen P0 corpus specifically for P4.
It exists to prevent obvious retrieval regressions while the larger benchmark
programme is built. It is deliberately small and must not be cited as proof
that SCOS is superior to another system.

The corpus covers exact memory lookup, semantic paraphrase, current-vs-stale
memory, exact code navigation, natural-language code retrieval, query-planner
intent, and mixed memory/code evidence.

`requires_semantic=true` identifies cases expected to benefit from BGE-M3 or
BGE-Code rather than lexical coincidence. `forbidden_ids` records evidence
that must not be surfaced as current truth.

CI validates structural integrity and the offline lexical regression gate below.
Actual deployed Recall@K, MRR, nDCG and latency still require a benchmark
runner against a configured retrieval deployment with git SHA and model-space
identity recorded.

## Deterministic offline quality gate (P0 partial)

Run from the repository root:

```bash
python3 -m unittest benchmarks.p4.test_fixture benchmarks.p4.test_offline_gate -v
python3 -m benchmarks.p4.offline_gate --check benchmarks/p4/lexical_baseline.json
```

This invokes the actual dependency-free `DeterministicLexicalBaseline` from
`benchmarks/harness.py` against every case in the frozen fixture. The
committed `lexical_baseline.json` records observed per-case rankings,
Recall@5/10/20, Precision@5/10/20, MRR, nDCG@5/10/20 and forbidden-at-5
exposure, with fixture SHA-256, engine version, base Git SHA, generator digest and deployment
class. CI rejects missing/extra cases, corpus changes, decreased per-case
quality, **any nonzero forbidden-at-5 exposure in either the current run or
its baseline**, and mismatched engine identity. Baseline recording also refuses
to write when forbidden evidence is retrieved in the top five; re-recording
cannot normalize a known-bad result. This is a narrow fixture-level safety
invariant, **not** tenant authorization, full temporal validity enforcement,
or production isolation. The gate never auto-updates the baseline. Explicitly
re-record after review with
`python3 -m benchmarks.p4.offline_gate --record-baseline benchmarks/p4/lexical_baseline.json`.

**Scope:** This is a regression gate for a deliberately tiny *lexical smoke*
corpus, not a test of FTS, vector, RRF, BGE, tenant isolation, live latency,
or production retrieval quality. It does not claim P0 completion or model
superiority. The 7 cases are easy for lexical overlap, so even 100% Recall@5
here is not meaningful superiority evidence. Full/live runs require an
explicitly configured deployment, frozen realistic corpora, model-space and
schema provenance, latency/cost measurements, and human-reviewed thresholds.
No live service is contacted by this gate.
