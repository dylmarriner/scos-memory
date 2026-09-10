# SCOS Memory Fabric — P0–P8 Engineering Roadmap

> **Canonical roadmap as of 2026-09-10.** Re-baselined against `main` through `b4727fbb584a12bd5e45c1a2dd1b99cfcfc20bf7`, including the BGE model-runtime merge (`847dbc21b52101df16ab9adc718b0a74ac3b8cbb`) and the subsequent DeepSeek resilience work (`957ab1dd0bcf0b8d3ea56ca409ec75559ab5418c`).
>
> `docs/IMPLEMENTATION_PLAN_P0_P3.md` is historical implementation detail for an older, narrower backlog. This file is the canonical P0–P8 roadmap.

## Goal

Build SCOS into a governed persistent-memory + code-intelligence substrate for AI agents that can answer:

- what do we know?
- why do we believe it?
- what evidence supports it?
- what code implements it?
- when was it true?
- what changed?
- what becomes stale or affected if something changes?

The target is one evidence system connecting durable memory, immutable code snapshots, retrieval, temporal history, graph structure, agent execution, and curated OKF knowledge without collapsing their authority boundaries.

---

# Current production and model baseline

The live deployment remains the Linode host documented in `docs/runbooks/production-access.md`.

```text
Internet / agent
      │
      │ HTTPS + public SCOS API key
      ▼
    Nginx
      │
      ▼
cloud-control-plane
      │
      │ validates customer key
      │ issues short-lived internal claims
      ▼
 mcp-gateway / governed services
      │
      ├── PostgreSQL + pgvector
      ├── OKF Git store
      ├── workers
      ├── DeepSeek-backed reasoning/synthesis paths
      └── internal model-runtime
               │
               ├── BGE-M3
               ├── BGE-reranker-v2-m3
               └── BGE-Code-v1
```

## Model responsibilities

DeepSeek is already part of SCOS and remains the generative/reasoning layer used by existing memory-intelligence and agent paths. BGE does not replace it.

```text
DeepSeek
├── reasoning
├── extraction
├── summarisation
├── synthesis
├── consolidation
├── OKF judgement
└── agent/code reasoning

BGE specialist retrieval layer
├── BGE-M3
│   └── memory/document dense embeddings
├── BGE-reranker-v2-m3
│   └── query-aware candidate reranking
└── BGE-Code-v1
    └── semantic code retrieval
```

Operational rules:

- Production SCOS runs on the Linode host, not the Raspberry Pi.
- Nginx remains the public ingress.
- Customer `scos_live_...` credentials terminate at `cloud-control-plane`.
- Internal model services do not receive customer API keys.
- `model-runtime` and raw inference backends do not receive public Nginx routes.
- BGE activation remains explicit through deployment configuration so an ordinary deploy cannot unexpectedly cold-load large models.
- DeepSeek remains independent from the BGE retrieval runtime.
- PostgreSQL and OKF authority boundaries remain unchanged by model choice.

---

# Non-negotiable architecture rules

1. **PostgreSQL is authoritative** for operational history, memory, code snapshots, evidence links, retrieval records, and governance state.
2. **OKF Git is authoritative** for reviewed curated knowledge. Model output never silently rewrites approved knowledge.
3. **Derived indexes are rebuildable.** Embeddings, FTS projections, graph scores, standing answers, and caches are projections, not authority.
4. **Embedding spaces are explicit.** Role, provider, model, revision, dimensions, and generation identify a vector space.
5. **No cross-model vector comparisons.** BGE-small, BGE-M3, BGE-Code, BGE-VL, or any later model spaces must never be mixed in one distance operation.
6. **Raw evidence survives learned ranking.** Reranking may reorder candidates but must preserve raw similarity, source rank, RRF contribution, timestamps, provenance, and governance scores.
7. **Failure degrades honestly.** An unavailable embedder or reranker contributes zero signal; SCOS must never fabricate “semantic” candidates.
8. **Tenant/RLS scope is mandatory.** Every new table and query path must preserve tenant/principal isolation.
9. **Code evidence is versioned.** Historical correctness is tied to immutable snapshot/symbol versions.
10. **Time is first-class.** Real-world validity time and SCOS belief/system time are separate.
11. **Benchmarks gate retrieval claims.** New models/rankers/planners must be measured against frozen baselines.
12. **Branches are not completion.** Roadmap work is complete only when merged to `main` and verified there, or explicitly abandoned.

---

# Phase status

| Phase | Objective | Current status |
| --- | --- | --- |
| **P0** | Benchmark and evidence baseline | **PARTIAL** |
| **P1** | Immutable snapshots + durable code store | **SUBSTANTIALLY COMPLETE** |
| **P2** | SCIP + AST / Tree-sitter extraction | **SUBSTANTIALLY COMPLETE** |
| **P3** | Incremental Git-aware indexing | **SUBSTANTIALLY COMPLETE; hardening remains** |
| **P4** | Hybrid memory + code retrieval | **IN PROGRESS; CURRENT PRIORITY** |
| **P5** | Temporal evidence | **PARTIAL** |
| **P6** | Unified memory + code graph | **PARTIAL** |
| **P7** | Stale-memory revalidation | **EARLY / MOSTLY UNBUILT** |
| **P8** | Impact intelligence | **CODE SIDE PARTIAL; CROSS-DOMAIN UNBUILT** |

---

# P0 — Benchmark and Evidence Baseline

**Objective:** make every retrieval/indexing/model change measurable.

## Already present

- `packages/benchmark-harness/`
- `docs/adr/0010-superiority-benchmark-methodology.md`
- code-intelligence baseline fixtures and integration tests
- audit artifacts separated from current implementation truth

## Remaining

- [ ] Versioned memory-retrieval corpus covering exact recall, paraphrase, long-history noise, conflicts, supersession, temporal queries, and scope isolation.
- [ ] Code-retrieval corpus covering exact symbol lookup, natural-language intent, cross-file relationships, rename continuity, and graph navigation.
- [ ] Historical impact corpus from real commits.
- [ ] Frozen baselines for FTS-only, vector-only, RRF, lexical code search, structural code search, and current `ChangeImpact`.
- [ ] Record Recall@5/10/20, Precision@K, MRR, nDCG@K, p50/p95 latency, candidate counts, and inference cost where relevant.
- [ ] Attach git SHA, schema version, model/revision/dimensions, corpus version, and deployment class to benchmark results.
- [ ] Add deterministic CI regression gates plus separately-invoked full/live benchmark runs.

## Completion gate

No P4+ ranking/model claim is accepted without a reproducible before/after benchmark.

---

# P1 — Immutable Snapshots and Durable Code Store

**Objective:** make repository state durable, reproducible, and queryable at immutable snapshot boundaries.

## Already present

Current `main` already includes:

- `database/migrations/000025_code_intelligence_persistence.sql`
- durable repositories, snapshots, files, symbols, symbol versions, and edges
- `parent_snapshot_id` lineage
- transactional snapshot activation
- branch/commit/extractor/configuration snapshot identity
- rename continuity through stable content identity
- durable v2 handlers in code-intelligence

## Remaining hardening

- [ ] Ensure snapshot identity/provenance is returned by every code query/impact response that needs historical reproducibility.
- [ ] Add explicit snapshot selection where callers still implicitly use the active branch snapshot.
- [ ] Verify retention/GC cannot remove code evidence referenced by memory-code links or audit records.
- [ ] Add interrupted-index/activation recovery tests around real Postgres transactions.
- [ ] Require every new P4+ code-derived record to reference immutable symbol/file/snapshot versions.

---

# P2 — SCIP + AST / Tree-sitter Extraction

**Objective:** extract high-confidence language-aware symbols and structural relationships.

## Already present

- Go-native extraction
- SCIP ingestion
- Tree-sitter fallback extraction
- symbol doc comments
- extraction confidence classes
- conservative called-name scope handling
- call-edge generation after declarations are known
- ambiguity avoidance rather than invented edges

## Remaining

- [ ] Publish language × extractor-tier capability matrix.
- [ ] Preserve confidence/provenance on structural edges as well as symbols.
- [ ] Improve type-resolved references where Tree-sitter can only provide syntax-level confidence.
- [ ] Maintain deterministic fixture repos per supported language.
- [ ] Keep authoritative SCIP facts distinct from heuristic extraction.

---

# P3 — Incremental Git-Aware Indexing

**Objective:** update code intelligence proportionally to changed content while preserving immutable history.

## Already present

`pgstore.go` already:

- records parent snapshots;
- compares parent/target file content hashes;
- skips re-parsing unchanged files;
- copies unchanged declarations forward;
- activates snapshots transactionally;
- preserves identity through unchanged-content renames.

## Remaining

- [ ] Git-diff-aware ingestion driver for parent→target commit worklists.
- [ ] Explicit add/modify/delete/rename/copy event metadata.
- [ ] Verify deleted symbols cannot leak into active target snapshots.
- [ ] Reduce unnecessary whole-snapshot structural recomputation where dependency boundaries make incremental recomputation safe.
- [ ] Persist indexing telemetry: parsed/reused files, symbols changed/reused, elapsed time, extractor tier, cache hit ratio.
- [ ] Benchmark incremental indexing against clean rebuild equivalence and changed-content scaling.

---

# P4 — Hybrid Memory + Code Retrieval

**Objective:** turn the already-merged model runtime and durable retrieval foundations into one correct, measurable hybrid retrieval system.

**This is the current highest-leverage phase.**

## P4 current reality

Memory retrieval already has:

1. namespace/RLS authorisation;
2. standing-answer lookup;
3. exact search;
4. PostgreSQL FTS;
5. vector search;
6. relation expansion;
7. recent-session retrieval;
8. RRF fusion;
9. deduplication;
10. authority/confidence ranking;
11. token allocation;
12. retrieval-run persistence.

Code intelligence already has:

- exact path/FQN lookup;
- lexical search;
- SCIP/AST/Tree-sitter structural data;
- graph traversal;
- PageRank-ranked impact.

The BGE runtime itself is **already merged to `main`** via PR #28 at `847dbc21b52101df16ab9adc718b0a74ac3b8cbb`. PR #26 is obsolete/superseded and must not be treated as active work.

Current runtime contract:

```text
retrieval-orchestrator ─┐
embeddings-worker ──────┼──> model-runtime
code-intelligence ──────┘       │
                                 ├── BAAI/bge-m3
                                 │     text/memory embeddings, 1024d
                                 ├── BAAI/bge-reranker-v2-m3
                                 │     relevance reranking
                                 └── BAAI/bge-code-v1
                                       code embeddings, expected 1536d
```

DeepSeek remains the existing reasoning/synthesis layer above retrieval and is not replaced by this runtime.

## P4.1 — Model-runtime integration

**Status: MERGED; production activation/pinning remains operational work.**

- [x] Replay BGE model-runtime work onto the then-current `main` rather than merging stale PR #26.
- [x] Preserve intervening embeddings-worker behavior.
- [x] Keep model-runtime internal-only.
- [x] Add hosted Compose model overlay.
- [x] Add explicit `SCOS_MODEL_RUNTIME_ENABLED` activation gate.
- [x] Add BGE-M3 text path.
- [x] Add BGE reranker endpoint.
- [x] Add BGE-Code endpoint.
- [x] Add focused runtime/worker/Compose CI.
- [x] Reject malformed required-backend flags.
- [x] Propagate configured model/revision into TEI startup.
- [ ] Pin validated production model revisions instead of mutable `main` revisions.
- [ ] Measure live Linode memory/CPU headroom before enabling additional simultaneous BGE profiles.
- [ ] Verify live deployment after explicit activation; source/CI proof is not live-infrastructure proof.

## P4.2 — Formal embedding spaces

**Status: PARTIAL. Worker migration safeguards exist; retrieval/store identity still needs completion.**

- [ ] Add explicit embedding-space identity: role + provider + model + revision + dimensions + generation.
- [ ] Filter memory vector retrieval by the active text space.
- [ ] Preserve BGE-small compatibility only as a separate legacy space.
- [ ] Make mixed-space vector comparison structurally impossible on normal query paths.
- [ ] Re-embed stale rows idempotently when the active model/revision changes.
- [ ] Add indexes appropriate to fixed-dimensional active spaces.
- [ ] Use the same space abstraction for code embeddings rather than inventing a second incompatible mechanism.

## P4.3 — Honest semantic degradation

**Status: NOT COMPLETE. Current retrieval still contains `vectorFallback`.**

- [ ] Remove `vectorFallback` as a semantic source.
- [ ] If query embedding fails, semantic candidate count is zero.
- [ ] If pgvector/vector search fails, semantic candidate count is zero.
- [ ] Exact/FTS/relation/session retrieval continues normally.
- [ ] Record semantic backend state in `retrieval_runs` so “zero matches” is distinguishable from “backend unavailable”.
- [ ] Add tests proving high-confidence unrelated memories are never relabelled as semantic matches.

## P4.4 — Query-aware BGE reranking

**Status: RUNTIME ENDPOINT MERGED; RETRIEVAL CONSUMER NOT COMPLETE.**

- [ ] Add model-runtime client to retrieval-orchestrator.
- [ ] RRF + dedup create a bounded candidate window.
- [ ] Send query + candidate text to `/v1/rerank`.
- [ ] Preserve raw lexical score.
- [ ] Preserve raw cosine/semantic similarity.
- [ ] Add separate RRF contribution/score field.
- [ ] Add separate reranker score field.
- [ ] Apply authority/confidence governance weighting after relevance reranking.
- [ ] On reranker failure, fall back explicitly to RRF + governance weighting.
- [ ] Record reranker model/revision/fallback state in retrieval-run metadata.
- [ ] Fix the current bug where `SemanticScore` is overwritten with the semantic lane's RRF contribution.

## P4.5 — Dense code retrieval with BGE-Code

**Status: RUNTIME ENDPOINT MERGED; DURABLE CONSUMER/STORAGE NOT COMPLETE.**

- [ ] Add durable code embedding storage keyed to immutable `code_symbol_versions` + embedding space.
- [ ] Add RLS/FORCE RLS/policy from table creation.
- [ ] Define deterministic code embedding document composition from FQN, symbol name/signature, doc comment, and useful source content.
- [ ] Add deterministic chunk/content identity for re-embedding.
- [ ] Embed changed/new symbol versions during or immediately after snapshot indexing.
- [ ] Query BGE-Code for natural-language→code retrieval.
- [ ] Fuse exact identifier + lexical FTS + dense BGE-Code + structural/SCIP + graph/centrality signals.
- [ ] Keep exact path/FQN as a precision anchor rather than letting learned ranking bury an exact identifier.
- [ ] Preserve per-signal provenance in returned code results.

## P4.6 — Query planning

**Status: NOT COMPLETE.**

Initial deterministic planner classes:

```text
exact/navigation
memory
code
mixed
temporal
```

- [ ] Planner controls eligible retrieval lanes and candidate budgets.
- [ ] Planner never controls or widens tenant/security scope.
- [ ] Exact/navigation requests avoid unnecessary model fanout.
- [ ] Code queries activate code retrieval lanes when repository context exists.
- [ ] Mixed queries may retrieve memory and code independently before cross-domain graph expansion in P6.
- [ ] Temporal classification prepares the request for P5 temporal filters but does not fake temporal semantics before P5 is complete.
- [ ] Benchmark planner decisions against an all-source baseline before making cost-saving decisions default.

## P4 completion gate

P4 is complete only when all of the following are true:

- [ ] Normal memory vector retrieval cannot compare mixed model/revision/dimension spaces.
- [ ] Embedding outages produce zero semantic candidates instead of fake semantic evidence.
- [ ] Raw semantic similarity is preserved independently from RRF and reranker scores.
- [ ] BGE-reranker is consumed by retrieval-orchestrator with explicit fallback behavior.
- [ ] BGE-Code embeddings are durable and tied to immutable symbol versions.
- [ ] Natural-language code retrieval fuses lexical, dense, structural, and graph signals.
- [ ] Query planning exists and cannot affect authorisation boundaries.
- [ ] P0 benchmarks demonstrate retrieval quality and record latency/cost trade-offs.
- [ ] Focused CI passes on the merged implementation.
- [ ] The final implementation is merged to `main` and verified there.

---

# P5 — Temporal Evidence

**Objective:** answer historical and supersession questions correctly across memory and code.

## Already present

- memory validity/observation/record-time fields
- memory versions and append-only correction/supersession history
- code snapshots tied to branch + commit
- parent snapshot lineage
- retrieval-run history

## Remaining

- [ ] Formalize valid time vs SCOS belief/system time.
- [ ] Add explicit system-time invalidation/supersession timestamps where missing.
- [ ] Add `as_of_valid_time` and `as_of_system_time` retrieval filters.
- [ ] Add explicit `as_of_commit` / snapshot selection to code-query paths that still assume active state.
- [ ] Ensure memory↔code evidence always references concrete historical symbol versions.
- [ ] Add temporal contradiction/supersession tests and P0 benchmark cases.

## Completion gate

SCOS correctly distinguishes “true now”, “true then”, “believed then”, and “implemented at commit X”.

---

# P6 — Unified Memory + Code Graph

**Objective:** operationalize the existing bridge between governed memory and immutable code evidence.

## Already present

`database/migrations/000033_memory_code_links.sql` provides the durable bridge:

```text
memory_items
    │
    └── memory_code_links
            ├── link_type
            │   ├── evidences
            │   ├── motivated_by
            │   └── invalidated_by
            ├── source
            │   ├── explicit
            │   ├── okf_promotion
            │   └── inferred
            ├── confidence
            └── code_symbol_version_id
                         │
                         ▼
                code_symbol_versions
```

## Remaining

- [ ] Complete governed API/MCP tooling for explicit memory↔symbol-version links.
- [ ] Validate tenant/scope ownership at link creation.
- [ ] Record actor/operation/turn evidence where schema metadata needs expansion.
- [ ] Infer high-confidence links from deterministic signals before any model inference.
- [ ] Add bounded memory→code and code→memory expansion.
- [ ] Use graph proximity as a ranking feature, not an authority override.
- [ ] Expose explainable provenance paths.
- [ ] Allow OKF proposals to reference concrete memory/code evidence without auto-mutating approved OKF state.

---

# P7 — Stale-Memory Revalidation

**Objective:** detect when code changes make linked memory/knowledge potentially stale and revalidate without silent rewriting.

## Prerequisites already present

- immutable snapshots
- incremental change detection
- symbol-version lineage
- memory version/supersession system
- `memory_code_links`

## Remaining

- [ ] Emit durable revalidation events when linked symbols change/delete.
- [ ] Resolve continuity into unchanged / renamed / changed / deleted / ambiguous states.
- [ ] Use deterministic classification first.
- [ ] For changed/ambiguous cases, use existing SCOS reasoning models to create a proposal, not an authoritative mutation.
- [ ] Outcomes: still-valid, needs-review, superseded, invalidated, unresolved.
- [ ] For memory, create governed new versions/supersession relations when approved.
- [ ] For OKF, create review proposals with old/new code evidence.
- [ ] Invalidate standing answers that depend on stale/superseded evidence.
- [ ] Measure false-positive/false-negative review outcomes.

---

# P8 — Impact Intelligence

**Objective:** answer “what is affected if this changes?” across code and knowledge.

## Already present

Code-side `ChangeImpact` already provides:

- reverse `CALLS` traversal;
- direct/transitive dependents;
- bounded depth;
- personalized PageRank ranking;
- integration coverage.

## Remaining

- [ ] Add high-confidence impact relationships beyond CALLS as extraction permits: imports, references/types, implementation/inheritance, tests, config ownership, generated links.
- [ ] Preserve relation confidence/provenance.
- [ ] Include snapshot/commit provenance in every result.
- [ ] Traverse from changed symbols through `memory_code_links` into affected facts, decisions, procedures, standing answers, and curated-knowledge references.
- [ ] Keep code impact and knowledge impact categories distinct.
- [ ] Return explainable paths for every affected object.
- [ ] Add governed “what-if” impact planning before edits are made.
- [ ] Compare predicted blast radius against actual later edits/test failures/commits and feed results back into P0 benchmarks.

---

# Cross-phase supporting work

## Safety and secret handling

- Add write-time secret/PII scanning before conversational facts or broadly promoted OKF material persist sensitive values.
- Keep credentials in security-gate/credential-vault boundaries rather than treating secrets as ordinary memory.

## Standing-answer completion

The standing-answer table/read path exists. Remaining work:

- evidence dependency tracking;
- writer/refresh generation;
- invalidation on evidence supersession/revocation/P7 staleness;
- freshness policy;
- stale-answer benchmark coverage.

## Agent execution-trace consolidation

Capture lessons from real execution:

```text
attempt → tool calls → errors → fix → verification → outcome
```

Consolidate these into evidence-backed procedures/negative memories without turning every raw tool event into authoritative memory.

## Multimodal retrieval

BGE-VL remains later work after P4 text/code retrieval is benchmarked and stable:

- extracted document/PDF text → BGE-M3 space;
- screenshots/page renders/diagrams → separate visual space;
- preserve page/artifact provenance;
- never compare visual/text/code vectors unless the model explicitly defines the same shared space and SCOS stores that identity explicitly.

## Cross-repository federation

Do not build federation before single-repository P4–P8 quality is proven. Repository identity must remain an explicit graph/snapshot boundary.

---

# Current execution order from `main`

Because P1–P3 are already substantially built and P4.1 is now merged, the practical order is:

1. **P4.2:** formal embedding-space identity and retrieval filtering.
2. **P4.3:** remove fake semantic fallback and add degraded-mode provenance.
3. **P4.4:** consume BGE reranker in retrieval while preserving raw/RRF/reranker/governance scores separately.
4. **P0 alongside P4:** freeze and expand retrieval benchmarks so the new ranking path is measurable.
5. **P4.5:** durable BGE-Code embeddings and hybrid code retrieval.
6. **P4.6:** deterministic query planning after the full-source baseline exists.
7. **P6:** operationalize memory↔code graph traversal and provenance.
8. **P5:** complete bitemporal/as-of semantics using connected evidence.
9. **P7:** stale-memory/OKF/standing-answer revalidation.
10. **P8:** extend existing code impact into cross-domain impact intelligence.
11. **P1–P3 hardening:** execute remaining items when they block or materially improve later phases; do not rebuild foundations that already work.

---

# Release gates

No phase is complete because code exists on a branch.

For each phase:

- [ ] focused unit tests pass;
- [ ] relevant Postgres integration tests pass;
- [ ] RLS/tenant isolation is exercised for new tables/queries;
- [ ] migrations are append-only and operationally safe;
- [ ] retrieval/model changes publish before/after P0 metrics;
- [ ] production topology remains consistent with `docs/runbooks/production-access.md`;
- [ ] source proof, CI proof, and live-deployment proof are labelled separately;
- [ ] implementation is merged to `main`;
- [ ] `main` is re-read after merge to verify the expected state actually landed.

## Immediate milestone

**Finish P4 on current `main`:** formal embedding spaces → honest semantic degradation → BGE reranking → BGE-Code durable hybrid retrieval → deterministic query planning, with P0 benchmark evidence alongside the work.

DeepSeek remains SCOS's existing reasoning/synthesis layer. BGE remains the specialist retrieval layer beneath it. The two are complementary, not competing stacks.
