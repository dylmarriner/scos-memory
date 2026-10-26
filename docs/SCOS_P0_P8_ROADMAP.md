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
| **P7** | Stale-memory revalidation | **PARTIAL: durable activation events and governed review listing merged; append-only review decisions remain in draft PR #64** |
| **P8** | Impact intelligence | **CODE IMPACT + GOVERNED MEMORY LINKS PARTIAL; FULL CROSS-DOMAIN ACCEPTANCE OPEN** |

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
- `benchmarks/p4/offline_gate.py`, `lexical_baseline.json`, and CI regression tests (isolated lexical smoke only; not full P0 qualification)

The P4 fixture is deliberately a deterministic smoke/regression corpus. It is not presented as proof that one model stack is superior.

**P0 offline quality-gate evidence (2026-10-26, review branch):**
An isolated runner executes the existing real deterministic lexical retriever
against all seven frozen P4 fixture queries, records ranked IDs, per-case
Recall@5/10/20, Precision@5/10/20, MRR, nDCG@5/10/20, and forbidden-at-5
exposure, and compares against a checked-in measured baseline. Six initial
subprocess/integration tests were RED before implementation and GREEN after.
CI now fails closed on corpus drift, missing cases, quality regression and
increased forbidden exposure; the baseline records fixture digest, engine,
version, checked-out base Git SHA, generator digest and deployment class. This is *only* an offline lexical smoke
regression gate. It is not BGE, FTS, RRF, tenant-isolation or live P0 quality
qualification. Full/live model baselines, realistic corpora, latency/cost,
and independent approval remain outstanding. Do not count this as P0 complete.

**P0 forbidden-evidence hardening (2026-10-26; PR #57):**
Three new RED-before-fix regression tests demonstrated that the lexical gate
could accept a nonzero forbidden-at-5 baseline, accept an equally contaminated
current result after rebaselining, and record a contaminated baseline outright.
The gate now rejects nonzero forbidden exposure in both current and baseline
results and refuses unsafe baseline recording; the measured seven-case corpus
remains zero-exposure. This protects only explicitly forbidden IDs in the
isolated smoke fixture, not tenant authorization, full temporal filtering,
realistic corpora or live retrieval. Review, merge and deployment gates remain.

**P0 measured-baseline integrity regression (2026-10-26; PR #57 review branch):**
Five new subprocess tests reproduced (RED) acceptance of corrupted baseline
evidence: lowered per-case scores, altered ranked IDs, out-of-corpus IDs,
altered aggregate quality and altered forbidden totals. The offline gate now
recomputes stored metrics from frozen relevance labels and the recorded ranking,
validates document IDs and duplicates, and verifies aggregate/forbidden totals
before any quality comparison. The five cases pass (GREEN), with measured
rankings, retrieval metrics, and forbidden exposure unchanged. This is only
integrity checking of the seven-case lexical smoke baseline; realistic P0
benchmarks, model comparisons, tenant isolation and live qualification remain
unverified. No production changes or authorization to merge/deploy.

**P0 forbidden-depth regression (2026-10-26; PR #57 review branch):**
The isolated lexical smoke gate previously checked forbidden evidence only in
the top five despite measuring retrieval at k=10 and k=20. A synthetic
six-document fixture demonstrated RED: a forbidden document ranked sixth and
baseline recording incorrectly succeeded. Four new tests cover rank-six
exposure, measured counts at k=5/10/20, contaminated rebaselining, and
inconsistent baseline counts. The gate now records and validates per-case and
aggregate forbidden exposure at all three depths, refuses to record any
nonzero exposure, and rejects contaminated current or baseline results.
Existing seven-case rankings and quality metrics are unchanged; the baseline
adds the two deeper-depth counters and refreshes generator provenance.
This remains isolated lexical smoke evidence, not live tenant/temporal
authorization, P0 qualification, or owner authorization to merge/deploy.


**P0 deterministic ranking-integrity regression (2026-10-26; PR #57 review branch):**
Two new regression tests reproduced (RED) a baseline-integrity gap: swapping
non-relevant retrieved IDs left all recorded quality metrics unchanged and
passed the gate, including when the changed baseline was loaded from disk.
The gate now requires exact ranked-ID equality for every deterministic lexical
case (GREEN). An intentional engine/ranking change therefore requires an
explicitly reviewed new baseline; no automatic rebaselining occurs. The
fixture, measured rankings, metrics and forbidden counts are unchanged;
only generator/base-commit provenance is refreshed. This remains a seven-case
offline lexical smoke check, not a deployed retrieval, tenant-isolation,
performance, or P0 completion claim. No merge or deployment authorized.

**P0 shared-harness provenance verification (2026-10-26; PR #57 review branch):**
Three RED-before-fix regression tests demonstrated that the lexical benchmark
omitted its imported `benchmarks/harness.py` implementation from provenance,
and accepted missing/forged harness digests when rankings were unchanged.
The measured baseline now records the shared-harness SHA-256, and the gate
rejects missing or mismatched fingerprints (GREEN). Existing seven-case
rankings, quality metrics, and forbidden-at-5/10/20 totals are unchanged.
This closes a narrow offline reproducibility gap only; deployed BGE/FTS/RRF,
latency/cost, tenant/temporal correctness, independent approval and deployment
gates remain outstanding. No merge or deployment authorized.

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
- [x] Add interrupted-index/activation recovery tests around real PostgreSQL transactions (including OS-process termination during file indexing, snapshot activation and historical reactivation; see evidence below).

**P1/P5/P8 historical impact selection (2026-10-26; review branch, not merged):**
The governed `POST /v2/code/impact` and `scos_code_impact` MCP surfaces
now accept `as_of_commit` or `snapshot_id` with an explicit `branch`.
The selected immutable generation is returned as `snapshot_id`, `commit_id`,
`branch` and `historical`. A shared bounded impact traversal rejects
cross-generation caller edges. Historical lookup rejects queued/failed,
cross-repository/tenant and mismatched-branch snapshots, and requires
`snapshot_id` when a commit has multiple extractor generations.
The historical impact still joins **currently visible and valid** governed
memory facts, explicitly labelled `knowledge_scope=current_visible_facts`;
this is not historical memory as-of-time filtering. A real PostgreSQL
integration test covers superseded-versus-current caller graphs, forged
cross-generation edges, memory namespace authorization, ambiguous commits,
invalid selection and queued generations. A separate RED→GREEN PostgreSQL
test proves that path queries matching at least 100 symbols fail closed
(HTTP 422) rather than returning a silently truncated impact graph. A gateway
forwarding test checks historical selectors and authenticated identity.
**P8 impact depth-budget regression (2026-10-26; PR #58 review branch):**
A real PostgreSQL regression reproduced an active impact request accepting
`max_depth=9` and returning HTTP 200 despite the API lacking a hard traversal
limit. Both current and historical impact now reject depth above 8 with
HTTP 422, and store entry points reject excessive depth even without HTTP.
The test checks the 8-hop boundary remains accepted.

**P8 impact node-budget regression (2026-10-26; PR #58 review branch):**
A real PostgreSQL test first reproduced current and historical one-hop impact
queries returning a 521-node graph without a total-node budget (RED). The
datastore now caps each snapshot-scoped caller query at 513 distinct rows and
fails closed when the full traversal exceeds 512 unique nodes, including seed
matches and cumulative two-hop expansion. HTTP returns 422 instead of a
silently partial blast radius. The test covers current and historical impact,
wide seed sets, one-hop and two-hop overflow, HTTP 422, and the exact 512-node
success boundary. Ten consecutive PostgreSQL regression runs, the uncached
full Go suite, and go vet passed locally. This closes the unbounded node-count
gap, **not** edge-density/SQL-work budgets, graph completeness, or cross-domain
impact qualification. Review, merge, deployed acceptance and recovery gates
remain outstanding.

**P8 impact edge-budget regression (2026-10-26; PR #58 review branch):**
A PostgreSQL 17 integration fixture with 65/66 visited nodes and 4096/4225
real CALLS edges demonstrated that depth and node budgets alone still allow
dense graphs to overwhelm the PageRank adjacency load (RED). Ranking adjacency
now uses SQL LIMIT 4097 and rejects any result exceeding 4096 CALLS edges
without returning a partial graph (GREEN). Current and immutable historical
queries both enforce the limit; HTTP returns 422. Exact-boundary and overflow
fixtures assert their persisted edge counts and verify the behavior. This
closes the unbounded ranking-edge transfer gap, **not** database query-work
budgets, latency/scale qualification, or full P8 acceptance. No production
deployment is authorized by this evidence.

**Not P1/P5/P8 completion:**
other active-only APIs, temporal memory semantics, retention/GC acceptance, production
deployment/recovery, independent review and merge remain open.

**P1 recovery hardening evidence (2026-10-25; merged via PR #55):**A new real-PostgreSQL regression suite exercises atomic rollback when indexing
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

**Additional P1 ownership/immutability hardening (2026-10-25; merged via PR #55):**
Real PostgreSQL 17 tests reproduced four independent failures before fixes:
`IndexSnapshot` accepted a different tenant's repository ID and persisted
foreign-owned file/symbol versions under the target snapshot; an already-active
snapshot could be indexed again with an empty file list, corrupting its stored
file count; a failed snapshot could be indexed and promoted to active; and
`CreateSnapshot` accepted a tenant ID that did not own its repository.
`IndexSnapshot` now locks and verifies the snapshot's repository and pending
status before any writes, and `CreateSnapshot` derives the tenant from an
ownership-constrained repository lookup. Additional concurrent-writer tests
exercise two index attempts on the same generation against real PostgreSQL.
These checks protect one-generation ownership and immutability but **do not**
establish cross-generation activation serialization, forced process termination,
full recovery, production readiness or completion of the P1 gate.

**P1 late-worker status race regression (2026-10-25; merged via PR #55):**
CodeRabbit identified a race where a late indexing/diff error could overwrite a
superseded, reactivatable generation as failed. A real PostgreSQL 17 test first
reproduced this corruption (RED), then passed (GREEN) after constraining
`SetSnapshotStatus` to pending states only. A second regression confirms
queued generations can still transition to failed for recovery. All 63
migrations applied to an isolated disposable database. This does not establish
process-kill or production recovery acceptance.

**P1 cross-generation activation race (2026-10-25; merged via PR #55):**
A deterministic real-PostgreSQL 17 integration test held the shared repository
row lock until two independent same-branch indexing transactions were
simultaneously blocked in `activate_code_snapshot`. Before the fix, both
committed as `active` (RED), violating the immutable snapshot/branch
activation invariant. Append-only migration
`database/migrations/000066_serialize_code_snapshot_activation.sql`
locks the shared repository row *before* superseding old generations, so
concurrent activations serialize and observe the previous commit (GREEN).
The test verifies exactly one active snapshot, pointer consistency, and
historical reactivation of the superseded generation. Migration number 000065
was introduced by PR #52, which merged on 2026-10-25; 000066 remains
distinct. This evidence covers cross-generation activation concurrency only; forced process termination,
full release/recovery drills, independent review, merge, and remaining P1
hardening are still required.

**P1 activation lock-order review regression (2026-10-25; merged via PR #55):**
A subsequent CodeRabbit major finding identified a repository/snapshot lock
inversion: the migration acquired the snapshot lock before the repository
lock, and file indexing, SCIP indexing and historical reactivation did the
same. A deterministic PostgreSQL 17 test held the repository row while
each of those three real application paths waited, then attempted to lock
the target snapshot. All three failed RED with PostgreSQL deadlock SQLSTATE
`40P01`. The migration now reads immutable repository ownership without a
snapshot lock, locks the repository first, and then revalidates the snapshot
under its row lock. The three Go paths share this same lock order before
any snapshot writes. All three contention scenarios now pass GREEN.
The existing cross-generation activation test was updated to observe the
earlier repository-lock wait as well as the activation-function wait.
A separate real-PostgreSQL SCIP regression verifies that foreign repository
IDs cannot inject symbols and active SCIP generations cannot be reindexed.
These tests do not substitute for forced process-termination, release/recovery
drills, independent approval, merge, or production deployment validation.

**P1 actual process-termination recovery regression (2026-10-25; merged via PR #55):**
The new `TestPGSnapshotProcessTerminationRollbackAndRecovery` launches the real
Go `IndexSnapshot` path in a separate operating-system process against disposable
PostgreSQL 17. A test-owned symbol row lock deterministically pauses the worker
*after* it has inserted the first file in its uncommitted transaction; the
parent confirms the exact blocked PostgreSQL backend via `pg_blocking_pids`,
then forcibly kills the worker. The test verifies that neither file nor symbol
versions from the killed transaction persist, the previously active snapshot
remains active, stale-generation startup recovery marks the queued generation
failed, and a replacement generation can index and activate both files. This
is a real process kill and database rollback, not a simulated indexing error.
It does **not** prove interruption during the activation function itself,
production restart behavior, restore/rollback drills, or complete P1 acceptance.

**P1 activation-boundary SIGKILL acceptance (2026-10-26; isolated AMD review branch):**
Two additional real PostgreSQL 17 integration tests launch the actual Go
indexing or historical-reactivation path in a separate OS process. A
test-owned row lock on the *previously active* generation forces the worker
to wait **inside** `activate_code_snapshot`, after the indexer has written
its files and marked the new generation ready within its uncommitted
transaction. `pg_stat_activity` and `pg_blocking_pids` identify the exact
blocked backend and owning lock before the worker is forcibly killed.
The tests verify that no partial versions, ready/active status, or repository
pointer changes survive the killed transaction; the historical reactivation
case verifies that neither old nor current immutable generation changes.
Startup stale-generation recovery, replacement indexing and a clean
historical reactivation retry are exercised. Both tests passed 20 consecutive
runs against disposable PostgreSQL 17. This completes the *isolated
PostgreSQL interruption-test* checklist item, **not** P1 release acceptance.
No live restart/rollback or production deployment is implied.

**Merge/CI reconciliation (verified 2026-10-26):** PRs #52, #53, #54 and
#55 are merged on GitHub, and `main` at `9969f8d` passed CI and P4 Hybrid
Retrieval on 2026-10-25 (runs 40187523312 and 40187523364). Earlier
paragraphs' merge/review caveats record the limitations at the time each
test was first introduced, not the present PR state. Independent approval
for this new test-only change and a separately owner-authorized deployment
remain outstanding. P1 provenance, retention/GC, deployed recovery and
the P0–P8 acceptance gates remain open.

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

**Review hardening evidence (2026-10-25, PR #54, pending review/merge):**
The snapshot delta now distinguishes unchanged-source copies from unambiguous
renames and preserves same-path modifications when file hashes collide.
A PostgreSQL 17 regression additionally covers SCIP-imported snapshots:
first document ingestion, unchanged re-index, changed declaration documentation,
and moved document paths. SCIP's persisted symbol hashes represent symbol
identity rather than source bytes, so SCIP-only deltas use a deterministic
per-document declaration fingerprint and report moves conservatively as
additions/removals, not proven file renames. The regression was observed RED
before the fix and GREEN against a disposable, fully migrated database.
This closes a review finding, **not** the remaining P3 Git-diff driver,
telemetry, equivalence, or scaling acceptance gates.

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

**P5 review evidence (2026-10-26, PR #60, unmerged):** Corrected the
successful empty-result path in `POST /v1/retrieve` so principals with no
readable namespaces still receive a durable retrieval-run audit with query hash,
identity, `as_of_valid_time`, and `temporal_scope`. The pre-fix real PostgreSQL
integration test failed with an empty `retrieval_run_id`; after the fix it
passes and checks the stored `retrieval_runs.filters` and empty evidence.
Verified on isolated AMD using disposable PostgreSQL 17/pgvector with 65
migrations: full uncached Go suite (51 packages), `go vet` for the retrieval
service, ten repeated focused integration runs, focused race detector and
diff validation passed. The migration-owner integration test did not assert
restricted-role behavior on this specific empty-result branch. Independent
review, merge, main-branch CI, historical system-time semantics and deployment
verification remain open; this evidence is **not** full P5 acceptance.

**P5 restricted-role acceptance expansion (2026-10-26, PR #60, unmerged):**
Added a real PostgreSQL integration test for the empty valid-time HTTP path
using the non-superuser, non-BYPASSRLS `scos_app` runtime role. It checks
successful audit creation, persisted tenant/principal/valid-time/scope, own
principal visibility, cross-principal and cross-tenant denial, and absence of
session-context leakage after connection reuse. The P4 PostgreSQL CI job now
applies the repository RLS policies after all migrations, provisions the
restricted role, and **requires** its DSN so the acceptance test cannot silently
skip. A RED test with the required role DSN absent failed as intended; the
GREEN test passed against an isolated PostgreSQL 17/pgvector instance after
65 migrations and 20 policies. This is branch-local test evidence; CI and
independent review of the new commit remain required, as do system-time history,
temporal contradiction/supersession cases, merge, and deployment verification.

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

## Implemented / review evidence (not final acceptance)

- [x] Emit durable snapshot-activation revalidation events for linked changed/deleted code.
- [x] Deterministic continuity classification with changed/deleted/ambiguous and safe-rename evidence (merged PR #63 additionally guards ambiguous copied successors).
- [ ] Governed human review decisions: **review branch only, unmerged.** The P7 decision slice adds an append-only, one-decision-per-event review record, namespace reviewer authorization (read + promote/administer), a rationale requirement, a restricted-role RLS policy, and a POST decision endpoint. It projects review outcomes separately from the original event status. **This is an assessment record only:** no fact, version, standing answer or OKF knowledge is automatically changed. A future correction/reopen mechanism remains necessary.

**P7 governed review decision TDD (2026-10-26; isolated AMD):**
A real PostgreSQL 17 integration test failed RED with HTTP 405 for the missing review operation. The implementation passed GREEN for authorized decision creation, read-only/outsider denial, invalid outcome/rationale rejection, duplicate-decision conflict, immutable source-event/fact preservation, and restricted `scos_app` role read/write and update denial. Codex review identified a valid Unicode JSON body-limit defect: a 2000-character escaped non-BMP rationale was RED (HTTP 400), then GREEN after increasing the bounded request limit to 32 KiB (enough for 2000 JSON-escaped surrogate pairs). Migration `000070_memory_revalidation_review_decisions.sql` and the role-grant policy protect the append-only evidence. This is branch-local evidence only until CI, independent review and merge.

**P7 review-finding remediation (2026-10-26; branch-only, unmerged):** CodeRabbit correctly identified that append-only review decisions obstruct parent event/tenant deletion. Migration `000071_revalidation_privileged_purge.sql` now adds a migration-owner-only, transactionally audited decision purge and an immutable tenant-independent ledger. The decision trigger rejects ordinary DELETE/UPDATE; `scos_app` cannot call the purge, delete decisions, or read the ledger. Real PostgreSQL tests first failed RED because the purge function was missing, then passed GREEN for restricted-role denial, transaction rollback, exact purge count and immutable audit evidence. `docs/P7_REVALIDATION_PURGE_RUNBOOK.md` documents the explicit-approval operator workflow. **Remaining review gate:** independent assessment of the privilege boundary, CI on the pushed commit, and validation of full tenant-offboarding dependencies outside P7; this function is not a general tenant-delete implementation. Distinct unsupported-outcome, short-rationale and >2000-character rationale cases were also added. No production tenant data was purged.

**P7 draft PR #64 main-sync verification (2026-10-26; isolated AMD):**
Reconciled the draft branch with main (including merged P7 ambiguous-copy guard
and P8 graph impact changes), preserving both roadmap evidence streams.
On a newly provisioned disposable PostgreSQL 17 database, all migrations
000001–000071 and all RLS policies applied successfully. Three repetitions
of focused governed-review/ambiguous-copy PostgreSQL tests passed with
mandatory restricted-role checks; the full uncached Go suite passed
(51 packages), as did `go vet ./...`. An earlier attempt against an older
throwaway database failed the ambiguous-copy test because that database
lacked migration 000069; fresh sequential migrations resolved the fixture
mismatch. This is **review-branch evidence**, not merged-main or production
qualification. PR #64 remains draft pending fresh CI and independent
privilege-boundary review; no production purge is authorized.

## Remaining

- [x] Emit durable revalidation events when linked symbols change/delete (migration 000064; snapshot activation).
- [x] Resolve continuity into unchanged / renamed / changed / deleted / ambiguous states (unchanged produces no event; ambiguous requires review).
- [x] Use deterministic classification first; see duplicate-copy hardening evidence below.
- [ ] Qualify continuity classification against labelled corpora and remaining edge cases.
- [ ] For changed/ambiguous cases, use existing reasoning models to create a proposal, not an authoritative mutation.
- [ ] Outcomes: still-valid, needs-review, superseded, invalidated, unresolved.
- [ ] Create governed memory versions/supersession relations only when approved.
- [ ] Create OKF review proposals with old/new code evidence.
- [ ] Invalidate standing answers depending on stale/superseded evidence.
- [ ] Measure false-positive/false-negative outcomes.

**P7 ambiguous-copy revalidation regression (2026-10-26; PR review branch,
not merged):** A real PostgreSQL 17 integration fixture linked an active
memory fact to a single original file, then replaced that file with two
identical-content copies in a new immutable snapshot. RED: the activation
trigger classified the transition as `renamed:still_valid`, even though
two successors made continuity ambiguous. Migration
`000069_revalidation_ambiguous_copy_guard.sql` adds a new-snapshot
content-duplicate check before treating a stable-symbol reuse as a safe
rename. GREEN: the event is `ambiguous:needs_review`; no memory fact is
rewritten. The focused revalidation tests passed ten consecutive runs
against a disposable migrated database, the full uncached Go suite and
`go vet ./...` passed, and focused race-detector tests passed twice.
This does not complete the human review/proposal lifecycle, standing-answer
invalidation, false-positive measurement, independent review, merge, or
deployment/recovery gates.

---

# P8 — Impact Intelligence

**Objective:** answer “what is affected if this changes?” across code and knowledge.

## Already present

Code-side `ChangeImpact` already provides reverse call traversal, direct/transitive dependents, bounded depth and personalized PageRank. P4 code search now also consumes persisted structural/SCIP graph signal during retrieval.

**P8 explainable reverse-CALLS evidence (2026-10-26; PR #58 review branch, not merged):**
A new real-PostgreSQL regression reproduced the missing multi-hop provenance:
linked memory facts on transitive callers exposed only a direct symbol-to-memory
link, with no proof of the caller chain from the queried seed. The implementation
now publishes `code_evidence_paths` keyed by immutable symbol-version ID and
builds each path from actual snapshot-bounded CALLS edges in reverse traversal
order (matched callee → caller → ...). `KnowledgeDependents[].EvidencePath`
appends the governed memory link to the same concrete chain. Shortest paths
are deterministic across edge-row/map ordering and cycles; missing paths fail
closed, and the existing 512-node/4096-edge caps still apply. Real PostgreSQL
tests cover current and historical code snapshots, 3-hop memory links and HTTP
serialization; unit tests cover tie-breaking, cycles and unreachable symbols.
This is **partial P8 path evidence only**, not P7 revalidation, relation-type
coverage, production qualification, or permission to merge/deploy.

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

**CI supply-chain gate hardening (2026-10-26; review branch, not merged):**
A successful GitHub CI run on PR #58 still contained a `govulncheck`
exit code 3: the CI runner used Go 1.26.9, affected by a `crypto/x509`
certificate verification vulnerability fixed in Go 1.26.10. The security
step's `continue-on-error` masked that finding. A separate review branch pins
Go 1.26.10 across GitHub workflows and the module toolchain, removes the
scan's false-green override, and adds CI regression tests for these contracts.
The Docker build stage already uses patched Go 1.27.3. This is a pending
security-gate correction, **not** evidence of an authorized production rebuild,
merge or deployment.


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
