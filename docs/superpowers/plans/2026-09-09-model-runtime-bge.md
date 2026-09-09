# SCOS BGE Model Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give SCOS a stable, governed model-runtime boundary for BGE-M3 text embeddings, BGE reranking, and BGE-Code embeddings while keeping heavyweight inference movable off the Raspberry Pi.

**Architecture:** SCOS services call a small Go `model-runtime` service. That service validates requests and responses and routes inference to Hugging Face Text Embeddings Inference compatible backends. The existing embeddings worker remains responsible for durable memory-vector backfill, but it now treats model, revision, and dimensions as one explicit embedding space and can obtain vectors through model-runtime.

**Tech Stack:** Go 1.26, Python 3.12, PostgreSQL/pgvector, FastEmbed 0.8+, ONNX Runtime, Hugging Face Text Embeddings Inference 1.9, Docker Compose.

**Spec:** `docs/superpowers/specs/2026-09-09-model-runtime-bge-design.md`

## Global Constraints

- `BAAI/bge-m3` is the default new text embedding model with 1024 dimensions.
- `BAAI/bge-reranker-v2-m3` is the reranking model.
- `BAAI/bge-code-v1` is the code embedding model with an expected 1536-dimensional output.
- Existing PostgreSQL, pgvector, RLS, memory governance, RRF, and code parsing remain authoritative.
- No vector comparison may cross model, revision, or dimension boundaries.
- The Raspberry Pi deployment must support remote inference rather than requiring all heavyweight models locally.
- Failure of optional semantic or rerank inference must degrade explicitly rather than fabricate a semantic signal.
- Do not add Kubernetes.

---

### Task 1: Stable model-runtime contract

**Files:**
- Create: `services/model-runtime/cmd/server/main_test.go`
- Create: `services/model-runtime/cmd/server/main.go`
- Create: `services/model-runtime/Dockerfile`

**Interfaces:**
- Consumes TEI-compatible `/embed`, `/rerank`, and `/health` endpoints.
- Produces `POST /v1/embed/text`, `POST /v1/embed/code`, `POST /v1/rerank`, and health endpoints.

- [x] **Step 1: Write contract tests** for text routing, dimension rejection, reranking order, invalid indexes, and empty input.
- [x] **Step 2: Trigger CI to verify the repository cannot yet satisfy the new contract.** Repo-wide `gofmt` currently fails before Go reaches this package, so the red state is recorded as blocked by pre-existing formatting defects rather than misreported as a focused test result.
- [x] **Step 3: Implement the minimal runtime service** with request size limits, finite-number validation, backend URL validation, model metadata, and the existing SCOS auth/control-plane middleware.
- [x] **Step 4: Add a minimal runtime container** built independently from the shared fabric image.
- [ ] **Step 5: Run focused formatting, vet, and tests** through a dedicated model-runtime CI job and require them to pass.

### Task 2: Correct memory embedding model-space handling

**Files:**
- Modify: `workers/embeddings/tests/test_worker.py`
- Modify: `workers/embeddings/src/scos_embeddings/worker.py`
- Modify: `workers/embeddings/Dockerfile`

**Interfaces:**
- Consumes `SCOS_MODEL_RUNTIME_URL`, `SCOS_INTERNAL_SERVICE_TOKEN`, `SCOS_CONTROL_PLANE_TOKEN`, `SCOS_EMBED_MODEL`, `SCOS_EMBED_MODEL_REVISION`, and `SCOS_EMBED_DIMENSIONS`.
- Produces only embeddings matching the active model/revision/dimension tuple for each memory version.

- [x] **Step 1: Write tests** proving source-content hashes, model/revision/dimension-aware backlog selection, and atomic stale-space replacement.
- [x] **Step 2: Implement model-space-aware backlog selection** so an old BGE-small vector does not satisfy a BGE-M3 backfill.
- [x] **Step 3: Correct `chunk_content_hash`** to SHA-256 of the exact source content.
- [x] **Step 4: Add model-runtime inference** with both internal bearer and control-plane tokens, strict returned-model metadata validation, and local FastEmbed fallback when runtime is unset.
- [x] **Step 5: Preserve legacy base-compose defaults** for `BAAI/bge-small-en-v1.5` at 384 dimensions while the optional model overlay enables BGE-M3/1024.
- [x] **Step 6: Upgrade FastEmbed constraint** to `>=0.8,<1` for BGE-M3 local fallback support.
- [ ] **Step 7: Run the focused Python worker tests** and require them to pass.

### Task 3: Deployable TEI model overlay

**Files:**
- Create: `deploy/compose/docker-compose.models.yml`
- Create: `docs/model-runtime.md`

**Interfaces:**
- Provides private TEI backends named `bge-m3`, `bge-reranker`, and `bge-code`, or accepts remote backend URLs over the private/Tailscale network.
- Connects `embeddings-worker` to `model-runtime` without changing the default base Compose deployment.

- [x] **Step 1: Add model-runtime to an optional Compose overlay.**
- [x] **Step 2: Add ARM64 CPU TEI profiles** for BGE-M3, BGE reranker, and BGE-Code with separate caches and no published raw backend ports.
- [x] **Step 3: Make runtime backend URLs overrideable** for remote x86/NVIDIA nodes.
- [ ] **Step 4: Add exact local-Pi and remote-GPU runbook commands** including Tailscale-only bind guidance and supported TEI image tags.
- [ ] **Step 5: Validate Compose syntax** independently of the broken repo-wide CI lane.

### Task 4: Query-aware reranking in memory retrieval

**Files:**
- Modify: `services/retrieval-orchestrator/cmd/server/main_test.go`
- Modify: `services/retrieval-orchestrator/cmd/server/main.go`

**Interfaces:**
- Consumes `SCOS_MODEL_RUNTIME_URL` and the runtime `/v1/rerank` response.
- Produces query-aware result ordering while retaining explicit RRF + governance fallback.

- [ ] **Step 1: Add a failing test** showing two RRF-close candidates are reordered by a runtime reranker response.
- [ ] **Step 2: Add a failing test** proving a reranker outage preserves RRF ordering rather than failing retrieval.
- [ ] **Step 3: Replace the fake semantic fallback** so unavailable vector inference contributes zero semantic candidates.
- [ ] **Step 4: Preserve raw semantic similarity separately from RRF contribution** instead of overwriting the semantic score field.
- [ ] **Step 5: Call `/v1/rerank` on the bounded top candidate window** before authority/confidence weighting.
- [ ] **Step 6: Run focused retrieval tests.**

### Task 5: BGE-Code dense retrieval

**Files:**
- Modify: `services/code-intelligence/cmd/server/*` durable v2 retrieval files and tests after locating the exact query implementation.
- Add a migration only if durable code vectors require a new fixed-dimension storage table.

**Interfaces:**
- Consumes `POST /v1/embed/code`.
- Produces dense code candidates that can be fused with exact, FTS, SCIP/symbol, and graph candidates.

- [ ] **Step 1: Locate the durable `/v2/code/*` query path and write a retrieval fixture** where lexical search misses a semantically matching function.
- [ ] **Step 2: Add fixed-space code vector persistence** keyed by snapshot/symbol version plus model/revision/dimensions.
- [ ] **Step 3: Backfill BGE-Code vectors idempotently.**
- [ ] **Step 4: Add dense candidate retrieval and fusion** without weakening exact/SCIP precedence.
- [ ] **Step 5: Run durable-store and tenant-isolation tests.**

### Task 6: Visual retrieval extension

**Files:**
- Separate follow-up service and migration after text/code retrieval benchmarks are green.

**Interfaces:**
- Will consume BGE-VL-large through a dedicated multimodal backend rather than pretending TEI text endpoints serve CLIP image inputs.

- [ ] **Step 1: Keep BGE-VL out of the initial runtime deployment** until an image-capable backend contract is defined and benchmarked.
- [ ] **Step 2: Add the visual embedding lane only after Tasks 1-5 are green.**

### Task 7: Verification and release gate

**Files:**
- Create: `.github/workflows/model-runtime.yml`
- Modify: PR #26 description when verification is complete.

**Interfaces:**
- Produces independent evidence for the new subsystem while existing repository-wide CI defects remain visible and unfudged.

- [ ] **Step 1: Add focused CI** running `gofmt` only on `services/model-runtime`, `go vet ./services/model-runtime/cmd/server`, `go test ./services/model-runtime/cmd/server`, and the embedding-worker unit tests.
- [ ] **Step 2: Build `services/model-runtime/Dockerfile`** in focused CI.
- [ ] **Step 3: Record remaining pre-existing global CI blockers** separately from failures introduced by this branch.
- [ ] **Step 4: Keep PR #26 draft until focused CI is green and Tasks 1-4 are integrated.**
