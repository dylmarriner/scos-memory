# SCOS Memory Fabric — P0–P8 Engineering Roadmap

> **Canonical roadmap as of 2026-09-10.** Repository state is authoritative. This roadmap distinguishes source implementation, benchmark acceptance, merge state, and live-production activation instead of treating them as one checkbox.
>
> `docs/IMPLEMENTATION_PLAN_P0_P3.md` is historical implementation detail. This file is the canonical P0–P8 roadmap.

> **Live/merge evidence update, 2026-10-25 NZ:** PR #29 merged on 2026-09-10. PR #48 (backup recovery, memory-history redaction, model/runtime and Docker CVE fixes) and PR #44 (x/crypto 0.58.0) are merged; latest main CI passed on commits `5d8d969` and `f840bea`. The Kubuntux SCOS backup manager was repaired after its SSHFS bind mount became stale; a new logical backup, physical backup, scratch PITR recovery, recovered Bitwarden decryption key, and OKF Git mirror all verified through SCOS's live APIs. The live backup plan reached **12/12** passing checks at 2026-10-24 22:03 UTC. **Release verification update (2026-10-25):** Kubuntux now runs the newly built shared image at source SHA `a697a7e6e4fd9aaf310a8b49daec9aa7252968e2` across **22 healthy Go services**, and the separate model-runtime and changed consolidation-worker images were rebuilt and deployed. The other workers, PostgreSQL and GPU BGE services were unchanged and remained running. All 30 project containers were running with no unhealthy healthchecked container in the post-rollout scan. The active image before rollout was saved as `scos-fabric:rollback-4f9e643` and its data was retained; the release worktree lives at `/run/media/kubuntux/backup-dev-ext4/scos/scos-memory-release`. Original developer checkout/branch was left untouched. The operator corrected a stale Docker bind to the SSHFS off-host backups directory, added mount-aware startup/rebind scripts with backups, preserved existing model-runtime TLS volume driver options, and passed real logical and physical backup, integrity, PITR, Bitwarden secret-recovery, and OKF Git mirror operations. The SCOS backup plan API reported **12/12 passing** after the rollout; restore-test cadence reported no overdue tests. **This is live deployment and backup evidence, not proof of full P0–P8 completion.** P0 quality baselines and P5–P8 acceptance remain open. The current documentation-only commit may post-date the deployed build SHA.

## Goal

Build SCOS into a governed persistent-memory + code-intelligence substrate for AI agents that can answer:

- what do we know?
- why do we believe it?
- what evidence supports it?
- what code implements it?
- when was it true?
- what changed?
- what becomes stale or affected if something changes?

The target is one evidence system connecting durable memory, immutable code snapshots, retrieval, temporal history, graph structure, agent execution and curated OKF knowledge without collapsing their authority boundaries.

---

# Current production and model baseline

The live deployment remains the Linode host documented in `docs/runbooks/production-access.md`.

```text
Internet / agent
      │ HTTPS + public SCOS API key
      ▼
    Nginx
      ▼
cloud-control-plane
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
               ├── BGE-M3
               ├── BGE-reranker-v2-m3
               └── BGE-Code-v1
```

## Model responsibilities

DeepSeek is already part of SCOS and remains the generative/reasoning layer. BGE is specialist retrieval infrastructure and does not replace DeepSeek.

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
- BGE activation remains explicit so an ordinary deploy cannot unexpectedly cold-load large models.
- DeepSeek remains independent from the BGE retrieval runtime.
- PostgreSQL and OKF authority boundaries remain unchanged by model choice.

---

# Non-negotiable architecture rules

1. **PostgreSQL is authoritative** for operational history, memory, code snapshots, evidence links, retrieval records and governance state.
2. **OKF Git is authoritative** for reviewed curated knowledge. Model output never silently rewrites approved knowledge.
3. **Derived indexes are rebuildable.** Embeddings, FTS projections, graph scores, standing answers and caches are projections, not authority.
4. **Embedding spaces are explicit.** Role, provider, model, revision, dimensions and generation identify a vector space.
5. **No cross-model vector comparisons.** BGE-small, BGE-M3, BGE-Code, BGE-VL or later spaces never mix in one distance operation.
6. **Raw evidence survives learned ranking.** Reranking may reorder candidates but cannot overwrite raw similarity, source rank, RRF contribution, provenance or governance scores.
7. **Failure degrades honestly.** An unavailable embedder/reranker contributes zero semantic signal; SCOS never fabricates semantic candidates.
8. **Tenant/RLS scope is mandatory.** Every new tenant-owned table and query path preserves tenant/principal isolation.
9. **Code evidence is versioned.** Historical correctness is tied to immutable snapshot/symbol versions.
10. **Time is first-class.** Real-world validity time and SCOS belief/system time remain separate.
11. **Benchmarks gate superiority claims.** New models/rankers/planners are measured against frozen baselines.
12. **Branches are not completion.** Work is complete only when merged to `main` and verified there, or explicitly abandoned.

---

# Phase status

| Phase | Objective | Current status |
| --- | --- | --- |
| **P0** | Benchmark and evidence baseline | **PARTIAL; P4 smoke corpus added** |
| **P1** | Immutable snapshots + durable code store | **SUBSTANTIALLY COMPLETE** |
| **P2** | SCIP + AST / Tree-sitter extraction | **SUBSTANTIALLY COMPLETE** |
| **P3** | Incremental Git-aware indexing | **SUBSTANTIALLY COMPLETE; hardening remains** |
| **P4** | Hybrid memory + code retrieval | **MERGED TO MAIN (PR #29 on 2026-09-10); frozen P0/live performance qualification still pending** |
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
- `benchmarks/p4/retrieval_fixture.json`
- `benchmarks/p4/test_fixture.py`
- `benchmarks/p4/README.md`

The P4 fixture is deliberately a deterministic smoke/regression corpus. It is not presented as proof that one model stack is superior.

## Remaining

- [ ] Expand the versioned memory-retrieval corpus for paraphrase, long-history noise, conflicts, supersession, temporal queries and scope isolation.
- [ ] Expand code retrieval with real repositories, natural-language intent, cross-file relations, rename continuity and graph navigation.
- [ ] Add historical impact cases from real commits.
- [ ] Freeze FTS-only, vector-only, RRF, lexical-code, structural-code and current ChangeImpact baselines.
- [ ] Record Recall@5/10/20, Precision@K, MRR, nDCG@K, p50/p95 latency, candidate counts and inference cost.
- [ ] Attach git SHA, schema version, model/revision/dimensions/generation, corpus version and deployment class to results.
- [ ] Run before/after P4 quality measurements with actual BGE backends.
- [ ] Add deterministic CI regression thresholds plus separately invoked full/live benchmark runs.

## Completion gate

No superiority/performance claim for P4+ is accepted without reproducible before/after evidence.

---

# P1 — Immutable Snapshots and Durable Code Store

**Objective:** make repository state durable, reproducible and queryable at immutable snapshot boundaries.

## Already present

- `database/migrations/000025_code_intelligence_persistence.sql`
- durable repositories, snapshots, files, symbols, symbol versions and edges
- `parent_snapshot_id` lineage
- transactional snapshot activation
- branch/commit/extractor/configuration snapshot identity
- rename continuity through stable content identity
- durable v2 handlers in code-intelligence

## Remaining hardening

- [ ] Return explicit snapshot identity/provenance from every historical code query/impact surface.
- [ ] Add explicit snapshot selection where callers still imply the active branch snapshot.
- [ ] Verify retention/GC cannot remove code evidence referenced by memory-code links or audit records.
- [ ] Add interrupted-index/activation recovery tests around real PostgreSQL transactions.

**P1 recovery hardening evidence (2026-10-25, PR pending review/merge):**
A new real-PostgreSQL regression suite exercises atomic rollback when indexing
fails after an earlier file insert, verifies that no partial file/symbol rows
or active-pointer changes survive, and retries the same immutable key after
explicitly marking/deleting the failed generation. It also covers startup
cleanup of aged queued generations without touching active state, refuses
reactivation of queued/failed/nonexistent snapshots, preserves idempotence
for already-active branches, and requires the workspace scan API to return
HTTP 202 (not a misleading HTTP 200 `unchanged=true`) for queued/indexing
snapshots. Both faulty responses were observed RED before implementation.
These are isolated integration results, **not** proof of process-kill,
concurrent-index, deployed recovery or production rollback readiness; the
P1 acceptance gate remains open pending those checks and review/merge.

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
- call/reference/implementation edge generation
- ambiguity avoidance rather than invented edges

## Remaining

- [ ] Publish a language × extractor-tier capability matrix.
- [ ] Improve type-resolved references where syntax-only Tree-sitter confidence is insufficient.
- [ ] Maintain deterministic fixture repositories per supported language.
- [ ] Keep authoritative SCIP facts distinct from heuristic extraction.

---

# P3 — Incremental Git-Aware Indexing

**Objective:** update code intelligence proportionally to changed content while preserving immutable history.

## Already present

`pgstore.go` already:

- records parent snapshots;
- compares parent/target file content hashes;
- skips re-parsing unchanged files;
- copies unchanged declarations and chunks forward;
- activates snapshots transactionally;
- preserves identity through unchanged-content renames.

## Remaining

- [ ] Git-diff-aware ingestion driver for parent→target commit worklists.
- [ ] Explicit add/modify/delete/rename/copy event metadata.
- [ ] Verify deleted symbols cannot leak into active target snapshots.
- [ ] Reduce unnecessary whole-snapshot structural recomputation where dependency boundaries permit it.
- [ ] Persist parsed/reused files, changed/reused symbols, elapsed time, extractor tier and cache-hit telemetry.
- [ ] Benchmark incremental indexing against clean rebuild equivalence and changed-content scaling.

---

# P4 — Hybrid Memory + Code Retrieval

**Objective:** combine the merged model runtime and durable retrieval foundations into one correct, explainable hybrid retrieval system.

## P4.1 — Model-runtime integration

**Status: MERGED via PR #28. Production activation/pinning remains operational work.**

- [x] BGE-M3 text endpoint.
- [x] BGE reranker endpoint.
- [x] BGE-Code endpoint.
- [x] Internal TLS/auth boundary.
- [x] Hosted Compose model overlay.
- [x] Explicit `SCOS_MODEL_RUNTIME_ENABLED` activation gate.
- [x] Focused runtime/worker/Compose CI definition.
- [x] Strict required-backend flag parsing.
- [x] TEI model/revision propagation.
- [ ] Pin validated production model revisions instead of mutable `main`.
- [ ] Measure live Linode memory/CPU headroom before enabling simultaneous profiles.
- [ ] Verify live model activation on the Linode after explicit rollout.

## P4.2 — Formal embedding spaces

**Status: IMPLEMENTED ON PR #29.**

- [x] Shared identity: role + provider + model + revision + dimensions + generation.
- [x] `memory_embeddings.embedding_space_id` with legacy backfill.
- [x] Generation-safe `(memory_version_id, embedding_space_id, chunk_number)` projection identity.
- [x] Worker resolves/creates the active space and retains historical spaces.
- [x] Memory vector SQL resolves and filters the exact active text space before pgvector distance operations.
- [x] Durable `code_embeddings` use the same space registry.
- [x] Code dense queries filter active snapshot + exact code space.
- [x] Mixed-space and superseded-snapshot regression coverage on real PostgreSQL/pgvector.

## P4.3 — Honest semantic degradation

**Status: IMPLEMENTED ON PR #29.**

- [x] Removed fake `vectorFallback` semantic behavior.
- [x] Query-embedding failure yields zero semantic candidates.
- [x] Vector-search failure yields zero semantic candidates.
- [x] Exact/FTS/relation/session retrieval continues independently.
- [x] Degraded model/source state is recorded rather than disguised as zero relevance.
- [x] Tests protect against relabelling unrelated high-confidence memories as semantic matches.

## P4.4 — Query-aware BGE reranking

**Status: IMPLEMENTED ON PR #29.**

- [x] Retrieval-orchestrator calls governed `model-runtime /v1/rerank`.
- [x] RRF + dedup create a bounded candidate window.
- [x] Query + candidate text are sent to the reranker.
- [x] Raw lexical score is preserved.
- [x] Raw cosine similarity is preserved.
- [x] RRF score is separate.
- [x] Reranker score is separate.
- [x] Authority/confidence governance weighting is applied after relevance reranking.
- [x] Reranker failure explicitly falls back to RRF + governance.
- [x] Model/revision/planner/degraded metadata is persisted with retrieval runs.
- [x] The previous SemanticScore/RRF overwrite bug is removed.

## P4.5 — Dense code retrieval with BGE-Code

**Status: IMPLEMENTED ON PR #29.**

- [x] Durable code embeddings reference immutable `code_symbol_versions` and explicit embedding spaces.
- [x] `code_embeddings` has tenant RLS + FORCE RLS from creation.
- [x] Deterministic embedding documents contain FQN/path/kind/signature/docs/useful persisted source content.
- [x] Content hashes support idempotent re-embedding.
- [x] New/changed active snapshots can backfill BGE-Code embeddings without invalidating the durable lexical snapshot when the model is unavailable.
- [x] Exact path/FQN remains a precision anchor and bypasses learned ranking.
- [x] Non-exact queries combine lexical + BGE-Code dense retrieval.
- [x] Persisted structural/SCIP edges participate as an independent retrieval signal.
- [x] Bounded personalized PageRank contributes graph/centrality signal.
- [x] Lexical, dense, structural/graph and fused score provenance remain distinct.
- [x] Active-snapshot, mixed-space and tenant-isolation behavior is integration-tested with PostgreSQL/pgvector.

## P4.6 — Deterministic query planning

**Status: IMPLEMENTED ON PR #29.**

Planner classes:

```text
exact/navigation
memory
code
mixed
temporal
```

- [x] Planner controls lane eligibility and candidate budgets.
- [x] Planner contains no tenant/principal/security scope and cannot widen authorization.
- [x] Exact/navigation avoids speculative model fanout.
- [x] Memory mode uses memory-oriented lexical/vector/relation/session lanes.
- [x] Optional durable `repository_id` + `branch` enables the code lane for repo-aware code/mixed requests.
- [x] Code evidence is returned separately from memory sections and selected immutable symbol-version IDs are recorded with the retrieval run.
- [x] Code-intelligence failure degrades only the code lane.
- [x] Standing-answer short-circuiting cannot suppress explicitly requested repo-aware code evidence.
- [x] `scos_retrieve` exposes optional durable repository context.
- [x] `scos_code_query` has a backward-compatible v1/v2 bridge; P4-aware callers use durable repository context while legacy clients remain functional.
- [x] Temporal classification prepares future P5 work without inventing temporal semantics.
- [ ] Compare planner cost/quality against an all-source live baseline under P0 before claiming efficiency gains.

## P4 source verification performed

The implementation has been exercised locally with:

- migration contract tests;
- retrieval gofmt/vet/unit tests;
- code-intelligence gofmt/vet/unit tests;
- all migrations applied from zero to PostgreSQL 17 + pgvector;
- real-PostgreSQL retrieval SQL tests;
- real-PostgreSQL code-intelligence integration tests;
- tenant RLS/FORCE-RLS tests using a non-superuser role;
- active-snapshot and mixed-space dense retrieval tests;
- exact-model-bypass and degraded-model handler tests;
- embeddings-worker tests in an isolated Python environment;
- P0/P4 frozen fixture structural tests;
- merged Compose structural validation.

GitHub Actions has also experienced runner-launch failures where jobs are created with no executed steps. Those are infrastructure failures and are not counted as product test failures or successful verification.

## P4 completion/acceptance gate

- [x] Normal memory vector retrieval cannot compare mixed embedding spaces.
- [x] Embedding outages produce zero semantic candidates instead of fake evidence.
- [x] Raw semantic similarity is preserved independently from RRF/reranker/final scores.
- [x] BGE reranker is consumed with explicit fallback behavior.
- [x] BGE-Code embeddings are durable and tied to immutable symbol versions.
- [x] Natural-language code retrieval fuses lexical, dense, structural/SCIP and graph/PageRank signals.
- [x] Query planning exists and cannot affect authorization boundaries.
- [x] A deterministic P0/P4 smoke corpus exists.
- [ ] Full P0 before/after quality, latency and cost evidence is recorded with actual model backends.
- [x] PR #29 merged to `main` on 2026-09-10; the resulting source was re-read and later CI runs verified it.
- [ ] Live Linode activation/capacity/model-pinning verification is complete before claiming BGE profiles are production-active.

---

# P5 — Temporal Evidence

**Objective:** answer historical and supersession questions correctly across memory and code.

## Already present

- memory validity/observation/record-time fields
- append-only memory versions and correction/supersession history
- code snapshots tied to branch + commit
- parent snapshot lineage
- retrieval-run history

## Remaining

- [ ] Formalize valid time vs SCOS belief/system time.
- [ ] Add explicit system-time invalidation/supersession timestamps where missing.
- [ ] Add `as_of_valid_time` and `as_of_system_time` memory filters.
- [ ] Add explicit `as_of_commit` / snapshot selection to code paths still assuming active state.
- [ ] Ensure memory↔code evidence references concrete historical symbol versions.
- [ ] Add temporal contradiction/supersession tests and P0 cases.

---

# P6 — Unified Memory + Code Graph

**Objective:** operationalize the existing bridge between governed memory and immutable code evidence.

## Already present

`database/migrations/000033_memory_code_links.sql` provides the durable bridge from `memory_items` to immutable `code_symbol_versions` with link type, source and confidence.

P4 additionally allows repo-aware retrieval to return code evidence beside memory evidence without collapsing the two authority domains.

## Remaining

- [ ] Complete governed API/MCP tooling for explicit memory↔symbol-version links.
- [ ] Validate tenant/scope ownership at link creation.
- [ ] Record actor/operation/turn evidence where schema metadata needs expansion.
- [ ] Infer high-confidence links from deterministic signals before model inference.
- [ ] Add bounded memory→code and code→memory expansion.
- [ ] Use graph proximity as a ranking feature, never an authority override.
- [ ] Expose explainable provenance paths.
- [ ] Allow OKF proposals to reference concrete memory/code evidence without mutating approved OKF state automatically.

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
- [ ] For changed/ambiguous cases, use existing reasoning models to create a proposal, not an authoritative mutation.
- [ ] Outcomes: still-valid, needs-review, superseded, invalidated, unresolved.
- [ ] Create governed memory versions/supersession relations only when approved.
- [ ] Create OKF review proposals with old/new code evidence.
- [ ] Invalidate standing answers depending on stale/superseded evidence.
- [ ] Measure false-positive/false-negative outcomes.

---

# P8 — Impact Intelligence

**Objective:** answer “what is affected if this changes?” across code and knowledge.

## Already present

Code-side `ChangeImpact` already provides reverse call traversal, direct/transitive dependents, bounded depth and personalized PageRank. P4 code search now also consumes persisted structural/SCIP graph signal during retrieval.

## Remaining

- [ ] Expand impact relationships beyond CALLS where extraction confidence permits: references/types, implementation/inheritance, tests, configuration ownership and generated links.
- [ ] Preserve relation confidence/provenance.
- [ ] Include snapshot/commit provenance in every impact result.
- [ ] Traverse changed symbols through `memory_code_links` into affected facts, decisions, procedures, standing answers and curated-knowledge references.
- [ ] Keep code impact and knowledge impact categories distinct.
- [ ] Return explainable paths for every affected object.
- [ ] Add governed what-if impact planning before edits.
- [ ] Compare predicted blast radius with later edits/test failures/commits and feed results into P0.

---

# Cross-phase supporting work

## Safety and secret handling

- Add write-time secret/PII scanning before conversational facts or broadly promoted OKF material persists sensitive values.
- Keep credentials in security-gate/credential-vault boundaries rather than ordinary memory.

## Standing-answer completion

The standing-answer table/read path exists. Remaining work:

- evidence dependency tracking;
- writer/refresh generation;
- invalidation on evidence supersession/revocation/P7 staleness;
- freshness policy;
- stale-answer benchmark coverage.

## Agent execution-trace consolidation

Capture real execution lessons:

```text
attempt -> tool calls -> errors -> fix -> verification -> outcome
```

Consolidate these into evidence-backed procedures/negative memories without treating every raw tool event as authoritative memory.

## Multimodal retrieval

BGE-VL remains later work after text/code retrieval is benchmarked and stable:

- extracted PDF/document text -> BGE-M3 text space;
- screenshots/page renders/diagrams -> separate visual space;
- preserve page/artifact provenance;
- never compare visual/text/code vectors unless a model explicitly defines one shared space and SCOS stores that identity.

## Cross-repository federation

Do not build federation before single-repository P4–P8 quality is proven. Repository identity remains an explicit graph/snapshot boundary.

---

# Current execution order

Following PR #29's 2026-09-10 merge and subsequent `main` verification, the practical sequence is:

1. **P0:** run full before/after P4 model-quality, latency and cost benchmarks; pin production-ready model revisions.
2. **P6:** operationalize memory↔code graph traversal and provenance.
3. **P5:** complete bitemporal/as-of semantics using connected evidence.
4. **P7:** stale-memory/OKF/standing-answer revalidation.
5. **P8:** extend code impact into cross-domain impact intelligence.
6. **P1–P3 hardening:** execute remaining items when they block or materially improve later phases.

P0 remains active throughout P5–P8 as the measurement layer rather than a one-time preliminary phase.

---

# Release gates

No phase is complete because code exists on a branch.

For each phase:

- [ ] focused unit tests pass;
- [ ] relevant PostgreSQL integration tests pass;
- [ ] RLS/tenant isolation is exercised for new tenant-owned tables/queries;
- [ ] migrations are append-only and operationally safe;
- [ ] retrieval/model performance claims publish P0 evidence;
- [ ] production topology remains consistent with `docs/runbooks/production-access.md`;
- [ ] source proof, CI proof and live-deployment proof are labelled separately;
- [ ] implementation is merged to `main`;
- [ ] `main` is re-read after merge to verify expected state actually landed.

## Immediate milestone

**P4 source is merged. Complete its full P0 reproducible acceptance programme, qualify current live BGE endpoints/performance and then advance the cross-domain P5–P8 acceptance cases.**

DeepSeek remains SCOS's reasoning/synthesis layer. BGE remains the specialist retrieval layer beneath it. They are complementary parts of the same stack.
