# SCOS Memory Fabric — Competitive Memory Capability Roadmap

**Status:** Proposed engineering overlay, 7 November 2026  
**Repository snapshot reviewed:** `main` at `9fe81e1e4bf688b122dd91909327607a8900ebe0` (latest commit returned by GitHub during this review; 4 November 2026).  
**Authority:** `GOVERNANCE.md`, accepted ADRs, the canonical [P0–P8 roadmap](SCOS_P0_P8_ROADMAP.md), and merged source on `main`.  
**Purpose:** Incorporate useful patterns observed in Hindsight, Mem0, Cognee and Graphiti without importing their architectures wholesale, duplicating SCOS capabilities, or creating competing sources of truth.

> This document is a capability overlay, not a replacement for P0–P8, a new governance authority, or evidence that any proposed item is implemented. Existing P0–P8 and §41 acceptance states remain unchanged. Source implementation, test evidence, merged-main evidence, and production activation must continue to be reported separately.

## 1. Executive direction

SCOS should not become a clone of four memory products. It already has a broader and more governed substrate: PostgreSQL authority, append-only events and versioned facts, evidence and promotion controls, entity and relation records, temporal validity, hybrid retrieval, a memory-intelligence service, durable code intelligence, and memory-to-code evidence links.

The competitive objective is narrower and more defensible:

**Make SCOS exceptionally good at retrieving, revising and explaining evidence across conversations, durable beliefs, entities, decisions, repositories, commits and code state — with temporal correctness, scope enforcement and reproducible proof.**

Borrow patterns, not dependencies:

- **Hindsight:** reflective synthesis, evidence-backed mental models, and useful conclusions derived across accumulated memories.
- **Mem0:** efficient fact extraction/update decisions, entity-aware retrieval, feedback signals and expiration semantics.
- **Cognee:** composable ingestion/enrichment pipelines, ontology-guided entity resolution, and selectable graph-retrieval strategies.
- **Graphiti:** rigorous temporal graph semantics, especially the distinction between when something was true and when SCOS learned, revised or withdrew it.

Do not add four external memory engines, four databases, or four parallel authority models. PostgreSQL and the existing SCOS evidence model remain canonical. Embeddings, graph projections, caches and ranking outputs remain rebuildable derivatives.

## 2. What already exists on the reviewed main snapshot

The following are source-backed capabilities, not proposed work:

| Capability | Current repository evidence | Roadmap implication |
|---|---|---|
| Governed fact lifecycle | Append-only fact versions, evidence links, correction, supersession, duplicate merge and revocation in `services/memory-intelligence/` | Extend the existing lifecycle; do not create a second fact store. |
| Reflection and belief revision | `reflection.go` groups supporting evidence, calculates support/opposition with noisy-OR, excludes inferred facts as belief evidence, and identifies stale beliefs with explicit reasons | Deepen semantic matching and reflection coverage; do not build a duplicate Reflect endpoint. |
| Entity and relationship model | Entity records, mention links, typed memory relations, hierarchy and diary endpoints | Improve resolution and retrieval use of these structures; do not introduce a second graph database by default. |
| Hybrid memory retrieval | Exact, full-text, vector, relation and session lanes; RRF; BGE reranking; explicit degraded-source/model reporting | Build on the existing retrieval orchestrator and benchmark each additional signal. |
| Code retrieval | Exact, lexical, dense BGE-Code, structural/SCIP and graph-PageRank signals, fused against immutable symbol versions | Do not list dense code embeddings or graph ranking as wholly missing. Focus on deeper cross-memory/code reasoning and coverage. |
| Memory/code provenance | `memory_code_links`, code-symbol-version references and reverse code-to-memory readback | Extend lineage and invalidation semantics rather than creating duplicate links. |
| Curated knowledge | OKF proposal/promotion path with governed review | Keep curated knowledge distinct from raw conversation memory. |
| Standing answers | Existing standing-answer storage/composition and freshness work | Improve invalidation and evidence refresh; do not recreate a cache. |
| Evaluation foundations | P0/P4 benchmark tooling and strict case-set/evidence integrity checks | Extend the harness with memory-specific and cross-code scenarios; do not invent a parallel benchmark framework. |

Relevant implementation references:
- [Reflection and belief revision](../services/memory-intelligence/cmd/server/reflection.go)
- [Memory intelligence service](../services/memory-intelligence/cmd/server/main.go)
- [Model runtime and retrieval architecture](model-runtime.md)
- [Durable code embedding store](../services/code-intelligence/cmd/server/dense_store.go)
- [Memory/code evidence readback](P6_MEMORY_CODE_READBACK.md)
- [P5 memory system events and temporal limitations](P5_MEMORY_SYSTEM_EVENTS.md)
- [Governed-learning ADR-0009](adr/0009-governed-learning-and-promotion.md)

## 3. Confirmed gaps and constraints

These are the gaps this overlay is intended to close or explicitly qualify:

1. **Entity-aware retrieval is not yet proven as a complete ranking path.** Entity mentions are persisted and the store comments describe retrieval boosting, but a comment is not an acceptance test. Verify the actual query/ranking path; implement a bounded, explainable entity-overlap signal if it is absent or incomplete.
2. **Memory graph expansion is limited.** Existing relation expansion provides useful graph context, but needs measured, bounded multi-hop traversal and typed path reasoning rather than unbounded graph walks.
3. **Reflection still has heuristic claim matching.** The current reflection engine is materially stronger than the early prototype: it computes support/opposition and evidence staleness. Its grouping and contradiction primitives still rely on deterministic lexical overlap/contradiction logic. Add semantic assistance without allowing a model to invent evidence or become the authority.
4. **Feedback is captured but not yet a proven learning loop.** `POST /v1/facts/{id}/feedback` records quality feedback. The current handler describes it as recording a signal without acting on it. Connect it to ranking only after sufficient scoped data and offline evaluation exist.
5. **Historical system-time retrieval remains deliberately unavailable.** `as_of_system_time` returns HTTP 501. Existing memory system events record database observation time, not transaction commit order; they do not reconstruct every historical approval, revocation, ACL change, redaction or deletion. Do not remove the 501 guard until the full historical belief ledger is trustworthy.
6. **Cross-code/memory reasoning is not yet the complete differentiator.** SCOS has code embeddings, graph ranking and memory/code links. The remaining opportunity is reliable multi-hop reasoning from a human decision or belief to the exact commit/symbol version that implements it, and detecting when later code changes invalidate that relationship.
7. **A continuous reflective-consolidation lifecycle needs explicit proof.** The repository has consolidation workers and an on-demand reflection API. A governed, event-triggered and scheduled reflect/review cycle should be specified and verified before it is described as autonomous sleep-time learning.
8. **No external comparative superiority claim is currently justified.** Existing benchmark integrity is valuable, but SCOS needs a reproducible, like-for-like memory benchmark against relevant baselines.

## 4. Prioritised workstreams

### MCI-0 — Establish a trustworthy memory benchmark baseline

**Priority:** P0 for competitive claims; prerequisite for learned ranking.

- Extend the existing benchmark harness rather than creating a separate runner.
- Freeze a versioned corpus containing multi-session recall, paraphrase, entity aliases, duplicate facts, contradictory facts, corrections, expiry, temporal questions, evidence provenance, cross-tenant isolation, multi-agent writes, and code/decision lineage.
- Include both answer-level tests and retrieval-level tests. A correct final answer must not hide a wrong or unauthorized retrieval path.
- Add measures for Recall@k, MRR/NDCG, evidence precision, contradiction precision/recall, belief-staleness accuracy, temporal answer accuracy, citation coverage, token use, p50/p95 latency, degraded-mode correctness and isolation violations.
- Record repository SHA, corpus digest, model/revision, embedding-space identity, prompts/policies, hardware, configuration and run artifacts.
- Compare SCOS with Hindsight, Mem0, Cognee and Graphiti only where the same task can be represented fairly. Record adapters, model calls and resource budgets. Publish raw results and limitations; never infer a score from a product README.
- Include deterministic no-LLM baselines and local-model runs so the evaluation remains usable without paid API keys.

**Acceptance:** frozen test cases; duplicate/missing case rejection; reproducible run manifests; independent baseline/candidate comparison; no cross-scope retrieval; no ungrounded answer counted as a hit.

### MCI-1 — Entity-aware and bounded graph retrieval

**Priority:** P4 retrieval extension.

- Verify whether entity-overlap scoring is active in the current memory retrieval ranking path. Add it only if source tracing and tests show it is absent or incomplete.
- Resolve aliases and canonical entity identity with tenant, namespace and sensitivity boundaries preserved. Ambiguous entity matches must remain ambiguous rather than being silently merged.
- Add bounded multi-hop expansion over typed relations such as `supports`, `contradicts`, `supersedes`, `derived_from`, `used_in` and code-evidence links.
- Add an explainable memory-graph centrality or personalized-PageRank signal where benchmarks show value. Keep graph score separate from lexical, semantic, RRF, reranker and governance scores.
- Bound depth, fan-out, candidate count and token cost. Apply authorization and temporal validity before graph candidates enter ranking.
- Preserve the reason and path for every graph-expanded result. Direct matches must not be displaced by weak, high-degree graph neighbours.

**Acceptance:** alias and entity-link fixtures; bounded traversal tests; tenant/sensitivity/valid-time isolation tests; explainable score decomposition; benchmark improvement without precision or latency regression.

### MCI-2 — Reflective consolidation and evidence-backed mental models

**Priority:** P5/P7 memory-intelligence extension; build on existing reflection and consolidation.

- Keep the existing deterministic reflection path as the safe baseline.
- Add an optional semantic claim-matching/contradiction stage for cases lexical overlap cannot resolve (for example, changed schedules, renamed systems, or incompatible numeric values). It may propose a relationship; it cannot create source evidence.
- Make reflection query-driven: retrieve the relevant evidence set first, then produce candidate conclusions, supporting evidence, opposing evidence, confidence, scope and freshness reasons.
- Add event-triggered reflection after meaningful events such as a correction, a contradiction, repeated corroboration, explicit feedback, task completion or a material code change.
- Add a bounded scheduled consolidation pass for namespaces with sufficient new evidence. Use novelty/surprisal and evidence-change signals to avoid repeatedly processing unchanged memories.
- Consolidate duplicates, stale beliefs, recurring preferences, project state and procedural learnings into reviewable candidates. Preserve raw episodes and evidence references.
- Reuse the existing promotion and review policy. Human-review classes and sensitive/high-impact knowledge must remain gated. No silent rewriting of approved OKF or shared knowledge.
- Persist concise conclusions, evidence and decision metadata, not hidden model chain-of-thought.

**Acceptance:** RED/GREEN regressions for lexical false positives/negatives; held-out semantic contradiction fixtures; evidence completeness; stale-belief detection; no inferred-only belief promotion; idempotent repeated reflection; bounded worker cost; review and rollback tests.

### MCI-3 — Complete temporal belief history before enabling system-time queries

**Priority:** P5 correctness blocker.

- Define and implement trustworthy system-time ordering. Wall-clock observation time and allocated event IDs are not commit order.
- Capture all state transitions required to reconstruct a belief at a prior system time: fact/version creation, correction, merge, supersession, revocation, approval/rejection, revalidation, relevant scope/ACL changes, redaction and authorised purge.
- Preserve valid time (when a fact applies in the world) separately from system time (when SCOS recorded or accepted that fact).
- Define transaction/commit ordering under concurrent writers and recovery. Specify how pre-ledger records are represented; never invent historical timestamps.
- Build point-in-time reconstruction from the authoritative ledger, not from current rows or a best-effort event projection.
- Add historical queries for facts, beliefs, relations and evidence, with the same authorization and sensitivity rules as current reads.
- Keep `as_of_system_time` HTTP 501 until migration, backfill boundary, concurrent commit ordering, deletion/offboarding, restore and restricted-role tests all pass.

**Acceptance:** concurrent transaction ordering tests; rollback and lost-acknowledgement tests; approval/revocation/ACL reconstruction; deletion/offboarding tests; historical answer fixtures; restricted-role tests; recovery and production rollout evidence. No historical query may return a plausible but incomplete reconstruction.

### MCI-4 — Unified decision-to-code memory lineage

**Priority:** P6 differentiator.

- Build on existing `memory_code_links`, immutable `code_symbol_versions`, repository snapshots and reverse readback.
- Standardise typed lineage from conversation/episode → fact/belief/decision → repository/branch/commit → symbol version and back.
- Support a query such as “why does this service behave this way?” by retrieving the current code plus the decision, evidence and historical context that explains it.
- Support the reverse query: “which decisions, beliefs or operational learnings are affected by this code change?”
- Add code-change invalidation/revalidation signals for linked memories. A changed symbol must not automatically erase a decision; it should mark the relationship for review and identify the exact changed snapshot/version.
- Use structural, dense, lexical and graph signals already present in code retrieval. Add richer edge types (for example calls, reads, writes, data-flow or configuration-dependency) only where extraction can establish them reliably.
- Require repository identity, branch and immutable snapshot/version binding. Do not treat a branch label or caller-supplied commit string as cryptographic Git attestation.
- Preserve separate code and memory IDs, provenance, ranking signals and authorization scopes in the response.

**Acceptance:** decision-to-symbol and symbol-to-decision fixtures; rename and supersession continuity; commit/snapshot pinning; changed-code revalidation; multi-repository isolation; end-to-end retrieval tests that prove both sides of the relationship.

### MCI-5 — Feedback-informed retrieval, safely

**Priority:** P7; only after MCI-0.

- Convert explicit positive/negative fact feedback and attributable retrieval outcomes into versioned learning signals.
- Keep feedback scoped to tenant, namespace, agent and task. Do not let one user's feedback silently change another user's ranking.
- Separate “retrieved”, “shown”, “used”, “cited”, “corrected” and “task succeeded”; these are not interchangeable labels.
- Learn bounded ranking adjustments from aggregate evidence only after minimum sample and confidence thresholds. Keep raw scores and governance weights visible.
- Evaluate candidate ranking policies offline, then shadow, canary and promote under ADR-0009. Keep a fast rollback pointer.
- Do not let a model grade its own answer and use that grade as the sole success signal.
- Preserve a deterministic fallback and avoid a per-query external LLM dependency.

**Acceptance:** feedback attribution tests; sparse-data no-op; namespace isolation; adversarial feedback/poisoning tests; offline replay; shadow/canary evidence; rollback; no regression in relevance, privacy, latency or token cost.

### MCI-6 — Composable memory enrichment pipeline

**Priority:** P8 candidate; only if pipeline complexity is justified by measured maintenance or quality gains.

- Formalise existing journal/outbox and worker stages as versioned, idempotent tasks: capture, redact, extract, resolve entities, classify, detect conflicts, link evidence, embed, index, reflect, propose promotion and verify.
- Make stages independently retryable and observable. A failed enrichment stage must not corrupt or hide the authoritative event.
- Support configurable extraction strategies and controlled ontologies for domain-specific entities/relations without requiring an LLM for every write.
- Record stage versions, inputs, outputs, provenance, errors and replay keys.
- Allow graph retrieval strategies to be selected by query class (direct relation, bounded expansion, decomposition, or centrality) only when evaluation proves the strategy useful.
- Reuse PostgreSQL, the transactional outbox and existing workers. Do not add Kafka, Kubernetes, a graph database or a second queue merely to imitate Cognee's pipeline shape.

**Acceptance:** deterministic pipeline replay; per-stage idempotency; failure/retry tests; schema/ontology versioning; no lost authoritative writes; measurable quality or operational improvement over the existing worker flow.

## 5. Recommended implementation order

1. **MCI-0 benchmark and source-truth matrix.** Establish the baseline before tuning.
2. **MCI-1 entity and bounded graph retrieval.** Low-risk extension of existing P4 retrieval.
3. **MCI-2 reflective consolidation.** Improve the existing reflection path and make background operation explicit and governed.
4. **MCI-3 temporal system history.** Treat as a correctness project, not a feature toggle. Keep the 501 guard.
5. **MCI-4 cross-code memory lineage.** Build the distinctive cross-domain capability on the existing P6 foundation.
6. **MCI-5 feedback learning.** Only after sufficient benchmark and production-quality signals exist.
7. **MCI-6 composable enrichment pipeline.** Defer unless evidence justifies the additional abstraction.

Work may proceed in parallel where dependencies allow, but MCI-0 must precede claims of improvement and MCI-5 must not precede trustworthy evaluation.

## 6. Non-negotiable engineering constraints

- PostgreSQL and approved OKF Git history remain the authorities; indexes and graph projections are rebuildable.
- No third-party memory engine becomes a second source of truth.
- No requirement for paid model APIs. Local inference and deterministic fallback remain supported.
- Authorization, tenant isolation, namespace membership and sensitivity filtering happen before retrieval/ranking/graph expansion.
- Every conclusion carries evidence, provenance, scope, confidence and temporal context.
- Models may propose extraction, matching or summaries; they cannot self-approve durable shared knowledge or change governance.
- Learned ranking and consolidation changes follow ADR-0009: observe, propose, offline replay, adversarial/failure tests, shadow, canary, approve or reject, and rollback.
- Missing models, unavailable indexes and worker failures are reported as degraded; they must never fabricate semantic evidence or convert a failure into “no matches”.
- No performance or superiority claim without reproducible measurements against a frozen baseline.

## 7. Definition of success

SCOS wins by reliably answering questions that require both durable human/project memory and the actual current or historical state of code — while showing exactly which evidence supports the answer, what changed, when it changed, which scope is authorised, and what remains uncertain.

Feature-count parity with Hindsight, Mem0, Cognee or Graphiti is not the goal. Evidence-backed cross-domain correctness is.
