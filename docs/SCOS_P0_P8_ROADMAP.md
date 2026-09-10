# SCOS Memory Fabric — P0–P8 Engineering Roadmap

> **Canonical roadmap as of 2026-09-09.** This document is re-baselined against current `main` at `2dbff506c8aa3aa182757cd687a1bb7ca343284a`, not the 2026-09-01 audit snapshot. `docs/IMPLEMENTATION_PLAN_P0_P3.md` remains historical implementation detail for an older, narrower backlog and is not the master roadmap.
>
> **Execution rule:** each unfinished phase is implemented through focused plans under `docs/superpowers/plans/`, with tests, migrations, RLS, benchmark gates, and deployment verification appropriate to the change. A roadmap checkbox is not a substitute for a design or test cycle.

## Goal

Make SCOS the strongest practical persistent-memory + code-intelligence substrate for AI agents by combining:

- durable, governed conversational memory;
- immutable, versioned code intelligence;
- semantic + lexical + structural retrieval;
- temporal evidence and belief history;
- explicit and inferred memory↔code provenance;
- stale-memory revalidation after code changes;
- evidence-backed impact intelligence;
- measurable retrieval quality rather than superiority-by-adjective.

The end state is one evidence system in which an agent can ask not only **“what do we know?”**, but also **“why do we believe it?”, “what code proves it?”, “when was it true?”, “what changed?”, and “what else is affected?”**

---

## Current production baseline

The live deployment is the Linode VPS documented in `docs/runbooks/production-access.md`. Roadmap work must preserve this topology unless a later deployment decision explicitly changes it:

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
      │ issues short-lived signed internal claims
      ▼
 mcp-gateway / governed services
      │
      ├── PostgreSQL + pgvector
      ├── OKF Git store
      ├── workers
      └── internal model-runtime
```

Operational constraints:

- Production SCOS runs on the Linode host, not the Raspberry Pi.
- Nginx remains the public ingress.
- Internal services are not published as public endpoints.
- Customer `scos_live_...` credentials terminate at `cloud-control-plane`.
- `model-runtime` and raw inference backends remain internal and do not get Nginx routes.
- The current host is finite-capacity infrastructure; model placement is decided by measurement, not wishful YAML.
- PostgreSQL and OKF authority boundaries must survive every later phase.

---

## Non-negotiable architecture rules

1. **PostgreSQL remains authoritative** for operational history, memory, code snapshots, evidence links, retrieval records, and governance state.
2. **OKF Git remains authoritative** for reviewed curated knowledge. Derived model output never silently mutates it.
3. **Derived indexes are rebuildable.** Embeddings, FTS projections, graph scores, standing answers, and caches may be regenerated from canonical state.
4. **No cross-model vector comparisons.** Embedding role, model, revision, dimensions, and generation define a vector space.
5. **Deterministic evidence survives learned ranking.** Reranking may reorder candidates but may not destroy raw similarity, source rank, provenance, timestamps, or governance scores.
6. **Failure degrades honestly.** An unavailable embedding/rerank backend contributes zero signal; SCOS must not invent “semantic” candidates from unrelated high-confidence records.
7. **Tenant/RLS context is mandatory** on every new data path and every new table receives RLS + FORCE RLS + a real policy from creation.
8. **Code facts are versioned.** Queries that claim historical correctness operate against an explicit repository snapshot/commit, not whatever happens to be active now.
9. **Time is first-class.** World-validity time and SCOS belief/system time are separate concepts.
10. **Benchmarks gate claims.** A new ranking/model/indexing strategy must beat frozen baselines on quality without unacceptable latency/cost regressions.

---

# Phase status

| Phase | Objective | Current repo status |
| --- | --- | --- |
| **P0** | Benchmark and evidence baseline | **PARTIAL** |
| **P1** | Immutable snapshots + durable code store | **SUBSTANTIALLY COMPLETE** |
| **P2** | SCIP + AST/Tree-sitter extraction | **SUBSTANTIALLY COMPLETE** |
| **P3** | Incremental Git-aware indexing | **SUBSTANTIALLY COMPLETE, gaps remain** |
| **P4** | Hybrid memory + code retrieval | **IN PROGRESS / highest leverage** |
| **P5** | Temporal evidence | **PARTIAL** |
| **P6** | Unified memory + code graph | **PARTIAL** |
| **P7** | Stale-memory revalidation | **EARLY / mostly unbuilt** |
| **P8** | Impact intelligence | **PARTIAL on code; cross-domain unbuilt** |

---

# P0 — Benchmark and Evidence Baseline

**Objective:** make retrieval/indexing/model changes measurable before further sophistication is layered on top.

## What exists now

- `packages/benchmark-harness/` provides ranking metrics used by the benchmark methodology.
- `docs/adr/0010-superiority-benchmark-methodology.md` defines the governance requirement for reproducible comparison rather than cherry-picked examples.
- Code-intelligence has fixed baseline fixtures in `services/code-intelligence/cmd/server/baseline_fixtures_test.go` and extensive integration tests.
- Current historical audits are now labelled as snapshots rather than current truth.

## Remaining work

- [ ] Create a versioned memory-retrieval corpus covering exact recall, paraphrase, temporal conflicts, supersession, noisy long-history queries, and scope isolation.
- [ ] Create a code-retrieval corpus covering exact symbol lookup, natural-language intent, cross-file call relationships, rename continuity, and graph navigation.
- [ ] Create a historical change-impact corpus from real commits: changed symbol(s) → actual downstream files/symbols touched by the commit or its follow-up fixes.
- [ ] Freeze baseline configurations for:
  - FTS only;
  - current memory vector retrieval;
  - current RRF pipeline;
  - current code lexical/structural retrieval;
  - current `ChangeImpact`.
- [ ] Record `Recall@5/10/20`, `Precision@K`, `MRR`, `nDCG@K`, p50/p95 latency, candidate count, and model/inference cost where applicable.
- [ ] Add benchmark result metadata: git SHA, schema version, model/revision, embedding dimensions, corpus version, and deployment class.
- [ ] Add a CI regression gate for small deterministic corpora and a separately-invoked production benchmark for full/live evaluation.

## Acceptance criteria

- Every P4+ retrieval/model change has an apples-to-apples baseline.
- Benchmark artifacts identify the exact source and model versions used.
- No “better than” claim is accepted without a reproducible run.

**Primary files:** `packages/benchmark-harness/`, `benchmarks/`, `docs/adr/0010-superiority-benchmark-methodology.md`, code-intelligence baseline/integration tests.

---

# P1 — Immutable Snapshots and Durable Code Store

**Objective:** make repository state durable, queryable, versioned, and reproducible at a commit/snapshot boundary.

## Current status: substantially complete

Current `main` already has the core architecture:

- `database/migrations/000025_code_intelligence_persistence.sql` defines durable repository/snapshot/file/symbol/edge storage.
- `services/code-intelligence/cmd/server/pgstore.go` creates repository snapshots with `parent_snapshot_id` lineage.
- Snapshot identity includes repository, branch, commit, extractor version, and configuration hash.
- Activation is transactional so partially indexed snapshots do not become current.
- File and symbol versions preserve snapshot/commit lineage.
- Content identity preserves continuity through file renames where content is unchanged.
- Durable query/impact handlers live in `pghandlers.go` rather than relying on the earlier in-memory store.

## Remaining hardening

- [ ] Make snapshot identity/provenance visible in every code-query response where it is not already returned.
- [ ] Add explicit snapshot selection to all code-query/impact interfaces that still assume “active branch snapshot”.
- [ ] Verify old snapshot retention/GC policy cannot remove evidence still referenced by memory-code links or audit records.
- [ ] Add recovery tests for interrupted snapshot creation and activation around actual Postgres transactions.
- [ ] Ensure every new code-derived table introduced in P4+ references an immutable symbol/file/snapshot version rather than a mutable logical symbol alone.

## Acceptance criteria

- The same snapshot ID always represents the same repository/branch/commit/extractor/config identity.
- Historical code queries remain reproducible after newer commits are indexed.
- No partially indexed generation can become active.

**Primary files:** `database/migrations/000025_code_intelligence_persistence.sql`, `services/code-intelligence/cmd/server/pgstore.go`, `pghandlers.go`, their integration tests.

---

# P2 — SCIP + AST / Tree-sitter Extraction

**Objective:** derive high-confidence symbols, declarations, references, and structural edges from real language-aware extractors.

## Current status: substantially complete

Current `main` contains:

- Go-native extraction in `services/code-intelligence/cmd/server/goextract.go` using Go parser/type-aware facilities where available.
- SCIP ingestion in `services/code-intelligence/cmd/server/scipimport.go`.
- Tree-sitter fallback extraction for supported non-Go languages in the code-intelligence package and language adapters.
- `database/migrations/000028_code_symbol_doc_comment.sql` for symbol documentation.
- `000029_code_symbol_confidence_class.sql` for extraction confidence/provenance.
- `000034_code_symbol_called_names_scope.sql` to preserve conservative call-resolution scope.
- `code_edges` call relationships generated after all declarations in a snapshot are known, avoiding per-file ordering errors.
- Ambiguous syntax-only call targets are deliberately omitted instead of fabricated.

## Remaining work

- [ ] Inventory language coverage and publish a capability matrix: language × extractor tier × symbols × references × calls × imports × docs.
- [ ] Preserve extractor provenance and confidence on every structural edge, not merely symbols.
- [ ] Improve type-resolved references for languages where the current Tree-sitter fallback can only provide syntax-level confidence.
- [ ] Add fixture repos for each supported language, with known expected symbols/calls and negative ambiguity cases.
- [ ] Keep SCIP as a first-class ingestion path when an authoritative external index exists; do not downgrade precise SCIP facts into heuristic equivalents.

## Acceptance criteria

- Structural edges carry enough provenance to tell SCIP/type-resolved facts from syntax-only inference.
- Ambiguous fallback extraction never masquerades as exact static analysis.
- Each supported language has deterministic regression fixtures.

---

# P3 — Incremental Git-Aware Indexing

**Objective:** update code intelligence proportionally to what changed while preserving immutable history and rename lineage.

## Current status: substantially complete, with Git orchestration gaps

Current `pgstore.go` already:

- records parent snapshot lineage;
- resolves the parent once per new snapshot;
- compares file content hashes against the parent snapshot;
- skips re-parsing unchanged files and copies their derived declarations forward;
- activates the new snapshot transactionally;
- uses stable content identity to preserve continuity through unchanged-content renames.

This is the core incremental-indexing mechanism that the older P0–P3 backlog described as missing. It is no longer missing.

## Remaining work

- [ ] Add a Git-diff-aware ingestion driver that can build the file worklist from parent→target commit rather than requiring callers to submit an undifferentiated full list.
- [ ] Distinguish add / modify / delete / rename / copy events explicitly in snapshot metadata.
- [ ] Verify deletion semantics: symbols removed from the target commit must not leak into active-snapshot queries through copied parent data.
- [ ] Optimize call-edge regeneration so a tiny change does not require needless whole-snapshot structural recomputation where safe dependency boundaries are known.
- [ ] Persist indexing telemetry: files parsed, files reused, symbols reused, symbols changed, elapsed time, extractor tier, and cache hit ratio.
- [ ] Add benchmarks showing indexing work scales with changed content rather than repository size for representative repos.

## Acceptance criteria

- An unchanged file is not reparsed.
- Deleted code cannot appear in the target snapshot.
- Rename lineage remains traceable.
- Incremental indexing produces the same query-visible target snapshot as a clean full rebuild.

---

# P4 — Hybrid Memory + Code Retrieval

**Objective:** make retrieval quality the strongest part of SCOS instead of merely the largest collection of services around it.

**Priority: highest current engineering leverage.**

## What exists now

Memory retrieval in `services/retrieval-orchestrator/cmd/server/main.go` already performs:

1. namespace/RLS authorisation;
2. standing-answer lookup;
3. exact search;
4. PostgreSQL full-text search;
5. vector search;
6. relation expansion;
7. recent-session retrieval;
8. RRF fusion;
9. deduplication;
10. deterministic authority/confidence reranking;
11. token-budget allocation;
12. retrieval-run recording.

Code intelligence already has exact/path/FQN lookup, lexical search, structural graph traversal, and PageRank-ranked impact. Dense code retrieval is the major missing lane.

## Model-runtime architecture

The active implementation direction is the internal SCOS-owned model runtime developed in PR #26, but that branch must be rebased onto current `main` before merge because its base has drifted.

Target contract:

```text
retrieval-orchestrator ─┐
embeddings-worker ──────┼──> model-runtime
code-intelligence ──────┘       │
                                 ├── BAAI/bge-m3
                                 │     memory/document dense embedding, 1024d
                                 ├── BAAI/bge-reranker-v2-m3
                                 │     query-aware relevance reranking
                                 └── BAAI/bge-code-v1
                                       code dense embedding, expected 1536d
```

The model runtime stays internal to the Linode/SCOS service network. It does not receive customer API keys and does not get a public Nginx route.

## P4 work order

### P4.1 Rebase and integrate model-runtime

- [ ] Rebase/re-implement PR #26 onto current `main` rather than resolving conflicts by preserving stale deployment assumptions.
- [ ] Keep the current Linode/Nginx/cloud-control-plane topology documented in the production runbook.
- [ ] Validate the production Compose invocation and host resource budget before enabling local model profiles.
- [ ] Pin production model revisions once validated; do not leave mutable `main` revisions as long-term production identity.

### P4.2 Formal embedding spaces

- [ ] Introduce an explicit embedding-space identity for memory and code vectors: role + provider + model + revision + dimensions + generation.
- [ ] Filter memory vector retrieval by the active text embedding space.
- [ ] Never compare BGE-small, BGE-M3, BGE-Code, or future BGE-VL vectors against each other.
- [ ] Re-embed stale rows idempotently after model/revision changes.
- [ ] Add indexes appropriate to each fixed-dimensional active space rather than relying on an unbounded mixed-space table forever.

### P4.3 Fix semantic degradation behavior

- [ ] Remove the current `vectorFallback` behavior that substitutes unrelated high-confidence memories when a query embedding/vector search is unavailable.
- [ ] On embedding failure, vector retrieval contributes zero candidates and the exact/FTS/relation/session lanes continue.
- [ ] Record degraded-mode metadata in retrieval runs so observability can distinguish “no semantic matches” from “semantic backend unavailable”.

### P4.4 Query-aware reranking

- [ ] Call `model-runtime /v1/rerank` on a bounded top candidate window after RRF/deduplication.
- [ ] Preserve raw source scores and RRF contribution separately from reranker score.
- [ ] Apply authority/confidence governance weighting **after** relevance reranking.
- [ ] On reranker failure, explicitly fall back to RRF + current governance weighting.
- [ ] Record reranker model/revision and degraded/fallback mode in `retrieval_runs` metadata.

### P4.5 Dense code retrieval with BGE-Code

- [ ] Add durable code embedding storage keyed to immutable symbol/file version + code embedding space.
- [ ] Embed symbol signature/name/doc-comment/body chunks with deterministic chunk identity.
- [ ] Add dense natural-language→code candidate retrieval.
- [ ] Fuse code signals: exact symbol/path, lexical FTS, SCIP/structural, dense BGE-Code, centrality, and graph proximity.
- [ ] Exact/authoritative structural matches remain precision anchors and cannot be demoted into unexplained model-only results.

### P4.6 Query planning

- [ ] Add an internal query-type planner instead of blindly paying for every retrieval lane on every request.
- [ ] Initial deterministic types: memory, code, mixed, temporal, exact/navigation.
- [ ] Planner output determines eligible sources and candidate budgets, not tenant/security scope.
- [ ] Benchmark planner decisions against the fixed all-source baseline before making it default.

## Acceptance criteria

- No mixed-dimensional/model-space vector operation is possible on the normal path.
- Embedding/rerank outages degrade without fake semantic evidence.
- BGE reranking improves P0 metrics over the current deterministic reranker on the frozen corpus.
- Natural-language code queries retrieve semantically relevant symbols that lexical-only search misses.
- Every returned candidate retains provenance for how it entered and how it was ranked.

---

# P5 — Temporal Evidence

**Objective:** answer historical and supersession questions correctly across both memory and code.

## What exists now

- Memory facts already carry multiple temporal fields such as observation/validity/record time in the memory-intelligence model.
- Memory versions and append-only correction/supersession history already exist.
- Code snapshots are explicitly tied to branch + commit and parent snapshot lineage.
- Retrieval records preserve query/run evidence separately from authoritative memory.

## Remaining work

- [ ] Formalize bitemporal memory semantics:
  - **valid time:** when the fact was true in the world;
  - **system/belief time:** when SCOS accepted, superseded, revoked, or stopped trusting it.
- [ ] Add the missing belief/system-time invalidation timestamp(s) rather than overloading `valid_until`.
- [ ] Extend retrieval filters for `as_of_valid_time` and `as_of_system_time`.
- [ ] Add code-query `as_of_commit` / explicit snapshot selection where still missing.
- [ ] Make memory↔code links snapshot-version-specific so historical questions do not jump to current code accidentally.
- [ ] Add temporal contradiction tests: old fact true at T1, superseded at T2, historical query at T1 must still return old evidence without presenting it as current truth.
- [ ] Add temporal benchmark cases to P0.

## Acceptance criteria

SCOS can correctly distinguish:

- “What is true now?”
- “What was true on date X?”
- “What did SCOS believe on date X?”
- “What code implemented this at commit Y?”

without mutating or erasing the history needed to answer those questions.

---

# P6 — Unified Memory + Code Graph

**Objective:** connect facts, decisions, procedures, symbols, snapshots, commits, evidence, and executions into one traversable provenance graph without collapsing their authority boundaries.

## What exists now

`database/migrations/000033_memory_code_links.sql` already provides a real RLS-protected bridge:

```text
memory_items
    │
    └── memory_code_links
            ├── link_type: evidences | motivated_by | invalidated_by
            ├── source: explicit | okf_promotion | inferred
            ├── confidence
            └── code_symbol_version_id
                         │
                         ▼
                code_symbol_versions
```

This means P6 is no longer “design a table”. The table exists. The job is to make it operationally useful.

## Remaining work

### P6.1 Explicit linking

- [ ] Verify/complete MCP/API tooling for agents to create an explicit memory↔symbol-version link.
- [ ] Validate the referenced fact and code symbol version belong to an authorised tenant/scope.
- [ ] Record actor, source operation/turn, and evidence context where the current schema is insufficient.

### P6.2 Automatic high-confidence links

- [ ] Derive links from deterministic signals before model inference:
  - symbol/FQN explicitly cited in a fact;
  - code query result used in the same turn that created a fact;
  - changed symbol + decision/procedure written in the same governed operation;
  - OKF promotion with concrete code evidence;
  - commit/snapshot identity captured in tool execution metadata.
- [ ] Only create `source='inferred'` links above a measured precision threshold.
- [ ] Keep inferred confidence and evidence separate from explicit links.

### P6.3 Retrieval and graph traversal

- [ ] Add memory→code and code→memory expansion to mixed queries.
- [ ] Bound expansion depth and candidate count to avoid graph explosions.
- [ ] Use graph proximity as a ranking feature, not an authority override.
- [ ] Expose provenance paths in explainability responses: fact → evidence link → symbol version → snapshot/commit.

### P6.4 Curated knowledge integration

- [ ] Allow OKF proposals to reference concrete memory/code evidence IDs.
- [ ] Never auto-edit approved OKF content when a linked symbol changes; create a revalidation proposal instead.

## Acceptance criteria

- A fact can explain which code version supports it.
- A symbol can explain which decisions/facts/procedures refer to it.
- Every inferred edge is labelled, confidence-scored, and reproducible from evidence.

---

# P7 — Stale-Memory Revalidation

**Objective:** detect when code changes make linked memory or curated knowledge potentially stale, then revalidate it without silently rewriting authority.

## Current status

The prerequisites now exist:

- immutable code snapshots;
- change detection/incremental indexing;
- symbol-version lineage;
- memory versions/supersession;
- `memory_code_links`.

What is largely missing is the revalidation workflow connecting them.

## Design

### P7.1 Staleness events

- [ ] After a new code snapshot activates, compare changed/deleted symbols with memory-code links targeting previous symbol versions.
- [ ] Emit a durable revalidation candidate/event containing:
  - tenant/scope;
  - fact/OKF reference;
  - old symbol version;
  - target snapshot/commit;
  - change kind;
  - structural/diff evidence;
  - reason for suspected staleness.

### P7.2 Revalidation engine

- [ ] Re-resolve the logical symbol in the new snapshot where possible.
- [ ] Classify link state deterministically first: unchanged, moved/renamed, changed, deleted, ambiguous.
- [ ] For changed/ambiguous cases, retrieve the old fact + old code evidence + new code evidence and ask the configured reasoning model for a **proposal**, never an authoritative mutation.
- [ ] Outcomes: still-valid, needs-review, superseded, invalidated, unresolved.

### P7.3 Knowledge consequences

- [ ] For conversational memory, create a new version/supersession relation when approved.
- [ ] For OKF, create a review proposal with exact code diff/snapshot evidence.
- [ ] Invalidate/refresh standing answers whose evidence set includes stale/superseded facts.
- [ ] Record revalidation latency and false-positive/false-negative review outcomes for tuning.

## Acceptance criteria

- A deleted/changed linked symbol can never silently leave a high-confidence memory looking freshly verified.
- No automated revalidation directly overwrites approved OKF authority.
- Rename-only changes do not create needless semantic invalidation when lineage proves continuity.

---

# P8 — Impact Intelligence

**Objective:** answer “what is affected if this changes?” across code, memory, decisions, procedures, standing answers, and curated knowledge.

## What exists now

Code-side impact intelligence is already meaningful:

- `pgCodeStore.ChangeImpact` walks `CALLS` edges backwards from matched symbols.
- direct and transitive dependents are separated;
- traversal depth is bounded;
- personalized PageRank ranks the visited subgraph around changed seeds;
- integration tests cover graph-impact behavior.

P8 therefore extends a working code engine rather than replacing it.

## Remaining work

### P8.1 Broaden code impact signals

- [ ] Add impact edges beyond `CALLS` as extraction confidence permits: imports, type/reference usage, inheritance/implementation, configuration ownership, tests, and generated relationships.
- [ ] Preserve confidence class per relation and prevent syntax-only edges from being presented as type-checked certainty.
- [ ] Include snapshot/commit provenance in every impact result.

### P8.2 Cross-domain blast radius

For changed symbol(s), traverse:

```text
changed code symbol
      │
      ├── code callers/dependents
      ├── memory_code_links
      │       ├── facts
      │       ├── decisions/procedures
      │       └── inferred/explicit evidence
      ├── standing answers using affected facts
      ├── OKF proposals/knowledge references
      └── tool/agent executions that depended on the symbol
```

- [ ] Return separate categories rather than one undifferentiated score.
- [ ] Rank by graph distance, centrality, link confidence, evidence authority, recency, and relation confidence.
- [ ] Add an explainable path for every affected object.

### P8.3 Change planning interface

- [ ] Extend the governed MCP/API surface with an impact-plan response suitable for coding agents before edits are made.
- [ ] Include likely tests/files, affected memory/knowledge, stale-answer risk, and unresolved uncertainty.
- [ ] Support “what if” analysis against a proposed symbol/file change without writing authoritative state.

### P8.4 Feedback loop

- [ ] Compare predicted impact to actual subsequent edits/test failures/commits.
- [ ] Store evaluation events without turning them into authority.
- [ ] Feed those outcomes back into P0 impact benchmarks and ranking calibration.

## Acceptance criteria

Given a proposed code change, SCOS can return a ranked, explainable blast radius covering both code and knowledge, and later measure whether that prediction was right.

---

# Cross-phase supporting work

These items support P0–P8 but are not promoted into fake extra phases.

## Safety / secret handling

- Add write-time secret/PII scanning before conversational facts or broadly-promoted OKF material persist sensitive values.
- Preserve current security-gate and credential-vault authority boundaries; do not stuff secrets into “memory because retrieval needs them”.

## Standing-answer completion

The read path and `memory_standing_answers` table exist. Complete the system by adding:

- evidence dependency tracking;
- writer/refresh generation;
- invalidation on evidence supersession/revocation/P7 staleness;
- freshness policy;
- benchmark coverage for hit accuracy and stale-answer prevention.

## Agent execution-trace consolidation

Capture durable lessons from actual work:

```text
attempt → tool calls → errors → fix → verification → outcome
```

Consolidate these into evidence-backed procedures/negative memories without making every raw tool event a “fact”. This should reuse existing turn/event/tool-execution authority rather than invent a second trace store.

## Multimodal retrieval

`BAAI/BGE-VL-large` remains a later extension once P4 text/code retrieval is benchmarked and stable:

- extracted PDF/document text → BGE-M3 space;
- page renders/screenshots/diagrams → separate BGE-VL visual space;
- never compare visual and text/code vectors directly unless the selected model explicitly defines a shared space and that space is stored as its own identity;
- visual evidence must retain source page/artifact provenance.

## Cross-repository federation

Do not build cross-repo graph federation before single-repository P4–P8 quality is proven. When it becomes a real use case, repository identity becomes another mandatory graph/snapshot boundary rather than a global namespace shortcut.

---

# Current recommended execution order

The original P0→P8 numbering describes capability layers, but current `main` has already completed much of P1–P3. The practical execution order from **today's repository** is therefore:

1. **P4.1–P4.4:** rebase model-runtime, formalize embedding spaces, remove fake semantic fallback, add real BGE reranking.
2. **P0:** freeze/expand benchmark corpora immediately alongside P4 so quality changes are measurable.
3. **P4.5:** BGE-Code durable dense retrieval and hybrid code fusion.
4. **P4.6:** query planning after the all-source hybrid baseline is measured.
5. **P6:** operationalize existing `memory_code_links` and provenance traversal.
6. **P5:** complete bitemporal/as-of semantics using the now-connected evidence model.
7. **P7:** stale-memory/OKF/standing-answer revalidation on code changes.
8. **P8:** extend existing code impact into cross-domain impact intelligence.
9. **P1–P3 hardening items:** execute opportunistically when their specific gaps block a later phase; do not rebuild the already-working foundations.

This order deliberately avoids spending months polishing already-strong snapshot/extractor machinery while retrieval quality and unified evidence remain the larger competitive gap.

---

# Release gates

No phase is considered complete solely because code exists.

For each phase:

- [ ] focused unit tests pass;
- [ ] relevant Postgres integration tests pass;
- [ ] RLS/tenant isolation is exercised for new tables/queries;
- [ ] `make audit` passes where the live stack is available;
- [ ] migrations are append-only and reversible operationally;
- [ ] retrieval/model changes publish before/after P0 metrics;
- [ ] production deployment topology remains consistent with `docs/runbooks/production-access.md`;
- [ ] source proof, CI proof, and live-deployment proof are labelled separately.

## Immediate next milestone

**P4 retrieval foundation on current `main`:** rebase the existing model-runtime work, then ship BGE-M3 embedding-space correctness + honest vector degradation + BGE reranking before adding BGE-Code dense retrieval.

That milestone gives every later phase a stronger retrieval substrate without invalidating the durable snapshot/extraction work already present in the repository.
