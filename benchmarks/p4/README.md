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

CI validates structural integrity only. Measured Recall@K, MRR, nDCG and
latency require a benchmark runner against a configured retrieval deployment;
those measurements belong to P0 and must include the git SHA and model-space
identity used for the run.
