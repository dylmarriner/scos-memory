# P0 contextual memory-retrieval corpus v1

`memory_retrieval_v1.json` is a **synthetic, versioned** expansion of the P0
memory-retrieval evaluation suite: 43 labelled documents and 30 queries across
paraphrase, long-history distraction, contradiction, supersession, valid-time,
tenant/namespace isolation, prompt injection and exact identifier retrieval.
Every document has explicit tenant, namespace and lifecycle state; each case
pins its expected and forbidden evidence IDs. The data is fictitious.

Run the **context-blind diagnostic**, not an acceptance benchmark:

```sh
python3 benchmarks/harness.py --suite benchmarks/p0/memory_retrieval_v1.json
```

The output labels the lexical baseline `context_aware: false` and counts the
unqualified contextual cases. Its ranking and timing are **not** evidence of
RLS, tenant isolation, valid-time correctness, BGE quality or production
latency. A case succeeds only when all required IDs are present and **no**
forbidden ID appears anywhere in the result. `forbidden_at_5/10/20` metrics
report exposure at measured ranking depths and are lower-is-better.

**Do not run this suite against a live tenant.** The current MCP gateway
benchmark adapter seeds all facts in one context and ignores per-case identity
and time selectors. `--endpoint` now rejects any contextual suite **before
seeding**; its direct recall adapter rejects contextual cases before network
access. A future isolated test-tenant adapter must enforce tenant, namespace,
valid-time and lifecycle semantics with real authenticated integration tests,
then record model/revision, schema, p50/p95 latency, cost and before/after
baselines. This dataset alone does not close P0 or P5 acceptance.

The corpus contract and mutation regressions run in `make test` via
`tests/test_p0_memory_corpus.py` and `tests/test_p0_contextual_evidence.py`.
Changing ground truth or document scopes requires a reviewed version bump.
