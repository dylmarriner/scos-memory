# P4 Hybrid Retrieval Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete P4 by making memory and code retrieval use explicit embedding spaces, honest degradation, BGE reranking, dense BGE-Code search, and a deterministic query planner on top of the BGE model-runtime already merged into `main`.

**Architecture:** PostgreSQL remains authoritative. `model-runtime` is the only query-time BGE inference boundary. Memory retrieval calls `/v1/embed/text` and `/v1/rerank`; code-intelligence calls `/v1/embed/code`. Persisted vectors are keyed by explicit embedding-space identity and immutable memory/code versions. Retrieval preserves raw source scores and provenance, then applies RRF, learned relevance, and governance weighting in distinct stages.

**Tech Stack:** Go 1.26, PostgreSQL 17 + pgvector, Docker Compose, existing SCOS auth/TLS packages, BAAI/bge-m3, BAAI/bge-reranker-v2-m3, BAAI/bge-code-v1.

**Spec:** `docs/SCOS_P0_P8_ROADMAP.md` (P4.2–P4.6)

## Global Constraints

- Preserve the Linode + Nginx + cloud-control-plane public boundary.
- DeepSeek remains the existing generative/reasoning path; P4 does not replace it.
- `model-runtime` stays internal and receives only SCOS internal/control-plane credentials.
- Never compare vectors from different role/provider/model/revision/dimension/generation spaces.
- Model/runtime failure contributes zero semantic signal; never fabricate semantic candidates.
- Every new data table carrying tenant-owned data has RLS + FORCE RLS + an explicit policy from creation.
- Code embeddings reference immutable `code_symbol_versions`, not mutable logical symbols.
- Preserve raw cosine/lexical/RRF/reranker scores separately.
- Exact code path/FQN matches remain authoritative precision anchors.
- No mocks/stubs/TODO/FIXME in shipped implementation.

---

### Task 1: Explicit embedding-space schema

**Files:**
- Create: `database/migrations/000036_embedding_spaces_and_code_embeddings.sql`
- Create: `database/migrations/000036_embedding_spaces_and_code_embeddings_test.go` only if the repository's migration-test convention supports per-migration Go tests; otherwise extend the existing schema/migration smoke test.

**Interfaces:**
- Produces `embedding_spaces(id, role, provider, model, model_revision, dimensions, generation, created_at)`.
- Adds `memory_embeddings.embedding_space_id` and backfills it from existing provider/model/model_revision/dimensions metadata.
- Produces `code_embeddings(id, tenant_id, symbol_version_id, embedding_space_id, chunk_number, chunk_content_hash, embedding, created_at)`.

- [ ] **Step 1: Write a failing schema test** asserting migration `000036` exists and contains `embedding_spaces`, `memory_embeddings.embedding_space_id`, `code_embeddings`, RLS, FORCE RLS, and a `code_embeddings` tenant policy.

```go
func TestP4EmbeddingSchemaMigration(t *testing.T) {
    migration := readMigration(t, "000036_embedding_spaces_and_code_embeddings.sql")
    for _, want := range []string{"CREATE TABLE IF NOT EXISTS embedding_spaces", "ADD COLUMN IF NOT EXISTS embedding_space_id", "CREATE TABLE IF NOT EXISTS code_embeddings", "ENABLE ROW LEVEL SECURITY", "FORCE ROW LEVEL SECURITY"} {
        if !strings.Contains(migration, want) { t.Fatalf("missing %q", want) }
    }
}
```

- [ ] **Step 2: Run the focused migration/schema test and confirm RED** because `000036` does not exist.
- [ ] **Step 3: Implement migration `000036`** with:
  - global non-secret model-space registry;
  - uniqueness on `(role, provider, model, model_revision, dimensions, generation)`;
  - `role IN ('text','code','visual')`;
  - backfill of one generation-1 text space for each distinct existing memory embedding tuple;
  - `memory_embeddings.embedding_space_id` FK + NOT NULL after backfill;
  - `code_embeddings` keyed by tenant + immutable symbol-version + space + chunk number;
  - tenant RLS policy using `current_setting('scos.tenant_id', true)`;
  - indexes on memory/code space IDs and code symbol versions.
- [ ] **Step 4: Run schema tests and confirm GREEN.**
- [ ] **Step 5: Commit:** `feat(p4): add explicit embedding spaces`.

---

### Task 2: Retrieval model-runtime client and honest semantic degradation

**Files:**
- Create: `services/retrieval-orchestrator/cmd/server/model_runtime.go`
- Create: `services/retrieval-orchestrator/cmd/server/model_runtime_test.go`
- Modify: `services/retrieval-orchestrator/cmd/server/main.go`

**Interfaces:**
- Produces `modelRuntimeClient` with `EmbedText(ctx,input) (EmbeddingResult,error)` and `Rerank(ctx,query,documents,topN) (RerankResponse,error)`.
- `EmbeddingResult` carries vector + model + revision + dimensions + role.
- `server` gains configured active text-space metadata and model-runtime client.

- [ ] **Step 1: Write failing tests** for:
  - parsing `/v1/embed/text` metadata and vector;
  - sending SCOS internal/control-plane headers to runtime;
  - rejecting empty/mismatched role/dimensions;
  - rerank response index validation;
  - `vectorSearch` returning zero semantic candidates when no query embedding exists rather than invoking a fallback.
- [ ] **Step 2: Run `go test ./services/retrieval-orchestrator/cmd/server` and confirm RED.**
- [ ] **Step 3: Implement `model_runtime.go`** using the current SCOS TLS/auth environment and bounded request bodies/timeouts.
- [ ] **Step 4: Modify retrieval startup** to read:
  - `SCOS_MODEL_RUNTIME_URL`;
  - `SCOS_MODEL_RUNTIME_CA_FILE`;
  - `SCOS_MODEL_TEXT_MODEL`;
  - `SCOS_MODEL_TEXT_REVISION`;
  - `SCOS_MODEL_TEXT_DIMENSIONS`;
  - `SCOS_MODEL_TEXT_GENERATION` default `1`.
- [ ] **Step 5: Replace query embedding through the embeddings-worker with direct `/v1/embed/text` runtime inference when configured.** Preserve request-supplied embeddings only when their model-space fields are supplied and match the active text space; otherwise reject them.
- [ ] **Step 6: Delete `vectorFallback`.** If embedding generation is unavailable, vector lane = empty. If pgvector query fails, log degraded semantic mode and return no semantic lane; exact/FTS/relation/session continue.
- [ ] **Step 7: Filter vector SQL** on `embedding_space_id` joined to `embedding_spaces` matching role/model/revision/dimensions/generation.
- [ ] **Step 8: Run focused Go tests and confirm GREEN.**
- [ ] **Step 9: Commit:** `feat(p4): make memory semantic retrieval space-safe`.

---

### Task 3: Preserve ranking provenance and add BGE reranking

**Files:**
- Modify: `services/retrieval-orchestrator/cmd/server/main.go`
- Modify: `services/retrieval-orchestrator/cmd/server/model_runtime_test.go`
- Modify: `database/migrations/000018_retrieval_explainability.sql` only if existing columns are insufficient; otherwise append new columns in `000036`.

**Interfaces:**
- `RetrievalResult` gains distinct `RawSemanticScore`, `RRFScore`, `RerankerScore`, and keeps `FusedScore` as final governed score.
- `rerankWithModel(ctx,query,[]RetrievalResult)` returns reordered scored candidates + model metadata + degraded flag.

- [ ] **Step 1: Write failing tests** proving:
  - raw semantic cosine survives RRF fusion unchanged;
  - `RRFScore` contains the sum of source reciprocal-rank contributions;
  - BGE reranker ordering is applied before authority/confidence governance boost;
  - runtime failure falls back to RRF + governance and reports degraded mode.
- [ ] **Step 2: Run focused retrieval tests and confirm RED.**
- [ ] **Step 3: Refactor `fuseResults`** so it never assigns RRF contribution into the raw semantic field.
- [ ] **Step 4: Add a bounded reranker candidate window** (default 50, env-configurable up to model-runtime max 128), using title + summary + content as the reranker document.
- [ ] **Step 5: Apply final score stages:**

```text
candidate source scores -> RRFScore -> BGE relevance ordering/score -> authority/confidence bounded governance boost -> FusedScore
```

- [ ] **Step 6: Persist ranking metadata** in `retrieval_runs.ranking_config/model_versions` and per-result provenance JSON: embedding space, reranker model/revision, planner mode, degraded model lanes, raw score fields.
- [ ] **Step 7: Run focused tests and confirm GREEN.**
- [ ] **Step 8: Commit:** `feat(p4): add BGE query-aware reranking`.

---

### Task 4: Durable BGE-Code embedding and dense code retrieval

**Files:**
- Create: `services/code-intelligence/cmd/server/model_runtime.go`
- Create: `services/code-intelligence/cmd/server/model_runtime_test.go`
- Modify: `services/code-intelligence/cmd/server/main.go`
- Modify: `services/code-intelligence/cmd/server/pgstore.go`
- Modify: `services/code-intelligence/cmd/server/pghandlers.go`
- Modify: `services/code-intelligence/cmd/server/pgstore_integration_test.go`
- Modify: `services/code-intelligence/cmd/server/pghandlers_integration_test.go`

**Interfaces:**
- `codeModelRuntimeClient.EmbedCode(ctx,input) (CodeEmbeddingResult,error)`.
- `pgCodeStore.UpsertCodeEmbedding(ctx,tenantID,symbolVersionID,space,chunk,vector) error`.
- `pgCodeStore.QueryDense(ctx,repositoryID,branch,queryVector,space,limit) ([]PGSymbolVersion,error)`.
- `PGSymbolVersion` gains `DenseScore` and retains `MatchType`/`Relevance` for lexical signal.

- [ ] **Step 1: Write failing unit tests** for code-runtime request/response validation and deterministic `codeEmbeddingDocument` construction from FQN/path/kind/signature/doc/chunk.
- [ ] **Step 2: Write failing Postgres integration tests** proving:
  - code embedding RLS isolates tenants;
  - mixed code spaces cannot participate in one dense query;
  - dense query returns active-snapshot immutable symbol versions only;
  - exact lookup remains exact even when dense results exist.
- [ ] **Step 3: Run focused code-intelligence tests and confirm RED.**
- [ ] **Step 4: Implement the internal BGE-Code client** using `/v1/embed/code` and the same SCOS internal TLS/auth conventions as retrieval.
- [ ] **Step 5: Add deterministic code embedding documents** containing FQN, path, kind, signature, doc comment, and stored chunk content. Hash the exact document and skip inference/upsert when the same symbol-version/space/hash already exists.
- [ ] **Step 6: Populate code embeddings after successful snapshot indexing** for symbol versions in the activated snapshot when the code runtime is configured. Failure is observable but cannot make the immutable lexical/structural snapshot invalid.
- [ ] **Step 7: Implement `QueryDense`** with active snapshot + exact embedding-space filters and cosine similarity.
- [ ] **Step 8: Update `/v2/code/query`:**
  - exact path/FQN hit => return exact results unchanged;
  - otherwise get lexical candidates + dense BGE-Code candidates;
  - fuse by RRF with distinct lexical/dense score fields;
  - use deterministic tie-breaking by FQN/path;
  - if BGE-Code is unavailable, return lexical results and `degraded_models` metadata rather than fake semantic matches.
- [ ] **Step 9: Run focused unit/integration tests and confirm GREEN.**
- [ ] **Step 10: Commit:** `feat(p4): add dense BGE-Code retrieval`.

---

### Task 5: Deterministic query planner

**Files:**
- Create: `services/retrieval-orchestrator/cmd/server/query_planner.go`
- Create: `services/retrieval-orchestrator/cmd/server/query_planner_test.go`
- Create: `services/code-intelligence/cmd/server/query_planner.go` only if code-specific routing cannot reuse simple intent rules locally.

**Interfaces:**
- `planQuery(query string, req RetrieveRequest) QueryPlan`.
- `QueryPlan.Mode` one of `exact_navigation`, `memory`, `code`, `mixed`, `temporal`.
- Plan controls eligible lanes/candidate budgets only; it never changes tenant/principal/namespace filters.

- [ ] **Step 1: Write failing table-driven tests** for exact/navigation, memory, code, mixed, temporal, and ambiguous queries. Ambiguous defaults to `mixed`/all safe lanes.
- [ ] **Step 2: Run focused tests and confirm RED.**
- [ ] **Step 3: Implement deterministic planner** from explicit request filters/intents and conservative lexical cues. No LLM call in the request planner.
- [ ] **Step 4: Apply planner to retrieval lane execution/candidate budgets** while preserving current all-source behavior for `mixed` and ambiguous inputs.
- [ ] **Step 5: Persist planner mode in retrieval-run metadata.**
- [ ] **Step 6: Run focused tests and confirm GREEN.**
- [ ] **Step 7: Commit:** `feat(p4): add deterministic retrieval planning`.

---

### Task 6: Compose wiring, focused CI, P0 regression fixture, and roadmap status

**Files:**
- Modify: `deploy/compose/docker-compose.models.yml`
- Modify: `.env.example`
- Modify: `.github/workflows/model-runtime.yml`
- Create: `.github/workflows/p4-hybrid-retrieval.yml` if keeping P4 checks separate makes failure ownership clearer.
- Create: `benchmarks/p4/retrieval_fixture.json`
- Create: `benchmarks/p4/README.md`
- Modify: `docs/SCOS_P0_P8_ROADMAP.md`
- Modify: `docs/model-runtime.md`

**Interfaces:**
- Model overlay injects model-runtime URL/CA/internal tokens and active text/code/reranker space metadata into retrieval/code-intelligence.
- CI validates migration, retrieval, code-intelligence, worker/runtime tests, and Compose merge for `text-model,rerank-model,code-model`.

- [ ] **Step 1: Add a small deterministic P0 fixture** with queries where lexical and semantic intent differ; document current baseline expectations without pretending it is a full superiority benchmark.
- [ ] **Step 2: Wire retrieval-orchestrator and code-intelligence to model-runtime in the model overlay.**
- [ ] **Step 3: Add env defaults** for generation, rerank candidate limit, and code embedding configuration.
- [ ] **Step 4: Extend focused CI** to run gofmt/vet/tests for retrieval + code-intelligence, migration/schema checks, Python embeddings tests, model-runtime tests, and `docker compose config` with all three profiles active.
- [ ] **Step 5: Update roadmap P4 checkboxes/status** only for behavior actually merged in this branch; keep production model activation clearly separate from source completion.
- [ ] **Step 6: Run/observe focused CI and fix every P4-owned failure.** Existing unrelated global CI failures must be identified by comparison with `main`, never waved away without evidence.
- [ ] **Step 7: Commit:** `docs(p4): mark hybrid retrieval implementation complete`.

---

### Task 7: Final branch verification and merge

**Files:** no new implementation files.

- [ ] **Step 1: Compare branch against latest `main`.** If `main` moved, rebase/replay before merge and rerun focused CI.
- [ ] **Step 2: Review final PR diff for:** cross-tenant scope, mixed vector spaces, public model exposure, fake semantic fallback, swallowed model errors, score-field corruption, and accidental DeepSeek changes.
- [ ] **Step 3: Require focused P4 CI GREEN on the final head.**
- [ ] **Step 4: Merge PR to `main`.**
- [ ] **Step 5: Reread `main` and verify the merge commit contains migration `000036`, retrieval runtime/reranking/planner integration, BGE-Code dense retrieval, Compose/CI wiring, and roadmap status.
- [ ] **Step 6: Close/supersede any obsolete P4 branches/PRs that the available GitHub connector can mutate.**
