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

CI validates structural integrity and the offline lexical regression gate below. The fixture loader also rejects blank queries/IDs, duplicate relevance or forbidden labels, non-boolean semantic flags, non-string document text, and corpus documents without any searchable fqn/path/text content; malformed inputs cannot be re-recorded as healthy evidence.
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
Recall@5/10/20, Precision@5/10/20, MRR, nDCG@5/10/20 and forbidden-at-5/10/20
exposure, with fixture SHA-256, engine version, base Git SHA, generator digest and deployment
class. CI rejects missing/extra cases, corpus changes, decreased per-case
quality, **any nonzero forbidden-at-5/10/20 exposure in either the current run or
its baseline**, and mismatched engine identity. Baseline recording also refuses
to write when forbidden evidence is retrieved in the top twenty; re-recording
cannot normalize a known-bad result. This is a narrow fixture-level safety
invariant, **not** tenant authorization, full temporal validity enforcement,
or production isolation. The gate also recomputes every stored baseline metric
from its recorded ranked IDs and frozen relevance labels, rejects unknown or
duplicate document IDs, and validates aggregate scores and forbidden totals at
all three depths. Both the fixture and recorded baseline are parsed as strict JSON: duplicate
keys at any depth (including relevance labels, forbidden IDs and rankings)
and non-finite NaN/Infinity values are rejected before scoring. Regression
tests reproduced acceptance of ambiguous evidence before the fix; rankings,
metrics and forbidden counts remain unchanged after regenerating only the
benchmark generator digest.
Lowering a stored score cannot silently weaken the regression
threshold. It requires the **exact deterministic ranking** to match the recorded
baseline, even when swapping irrelevant results would leave Recall/MRR/nDCG
unchanged. Intentional ranking changes require explicit review and
requalification. The recorded `git_base_sha` is resolved from the SCOS source checkout, not
from the invoking process's current directory. A real CLI regression runs
from an unrelated Git repository, including a foreign `GIT_DIR` override,
to guard against false commit attribution.
This remains source provenance, not proof of deployment identity.
The gate never auto-updates the baseline. Explicitly
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

**P0 depth guard (2026-10-26):** The initial top-five-only guard allowed a forbidden
document at rank six to be recorded as safe, despite evaluating Recall@10/20.
The gate now records and rejects forbidden evidence at k=5, 10 and 20,
including baseline consistency checks. The synthetic rank-six case was RED
before the change and GREEN afterward. This still does not enforce tenant
authorization or temporal correctness in a deployed retrieval service.

**P0 shared-harness provenance guard (2026-10-26; PR #57 review branch):**
Three RED-before-fix tests established that the measured record lacked a digest
of `benchmarks/harness.py` and that a baseline with missing or forged harness
provenance passed even when its rankings and metrics were identical. The
recorded baseline now contains `harness_sha256`, computed from the exact
retrieval/scoring implementation, and `--check` requires it to match.
This catches shared-engine source drift even when the tiny corpus happens to
produce unchanged results. Re-recording requires explicit review. The
seven-case fixture, measured rankings, metrics and forbidden counts are
unchanged. This is not a live deployment or model-space qualification.
