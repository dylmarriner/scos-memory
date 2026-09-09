# SCOS BGE Model Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give hosted SCOS a stable, governed internal runtime for BGE-M3 text embeddings, BGE reranking, and BGE-Code embeddings while preserving the existing Linode + Nginx + cloud API-key ingress.

**Architecture:** External clients continue to call the public SCOS URL through Nginx and `cloud-control-plane`. Customer `scos_live_...` API keys terminate there and are replaced with short-lived signed internal claims. Internal SCOS services call a Go `model-runtime` service, which validates requests/responses and routes inference to TEI-compatible model backends. The embeddings worker remains responsible for durable memory-vector backfill and enforces model/revision/dimension consistency.

**Tech Stack:** Go 1.26, Python 3.11+, PostgreSQL/pgvector, FastEmbed 0.8+, ONNX Runtime, Hugging Face Text Embeddings Inference 1.9, Docker Compose, existing Nginx reverse proxy.

**Spec:** `docs/superpowers/specs/2026-09-09-model-runtime-bge-design.md`

## Global Constraints

- `BAAI/bge-m3` is the default new text embedding model with 1024 dimensions.
- `BAAI/bge-reranker-v2-m3` is the reranking model.
- `BAAI/bge-code-v1` is the code embedding model with expected 1536-dimensional output.
- Existing PostgreSQL, pgvector, RLS, memory governance, RRF, cloud API-key ingress, and code parsing remain authoritative.
- No vector comparison may cross model, revision, or dimension boundaries.
- No model-runtime or raw inference endpoint may be exposed through Nginx.
- Customer API keys terminate at `cloud-control-plane`; model services use internal SCOS authentication only.
- Failure of optional semantic/rerank inference must degrade explicitly rather than fabricate semantic signal.
- Production deployment is `/opt/scos-memory-fabric` on the Linode VPS through `deploy/scripts/auto-deploy.sh`.
- Do not add Kubernetes.

---

### Task 1: Stable model-runtime contract

**Files:**
- Create: `services/model-runtime/cmd/server/main_test.go`
- Create: `services/model-runtime/cmd/server/main.go`
- Create: `services/model-runtime/Dockerfile`

**Interfaces:**
- Consumes TEI-compatible `/embed`, `/rerank`, and `/health` endpoints.
- Produces internal `POST /v1/embed/text`, `POST /v1/embed/code`, `POST /v1/rerank`, and health endpoints.

- [x] **Step 1: Write contract tests** for text routing, dimension rejection, reranking order, invalid indexes, and empty input.
- [x] **Step 2: Record initial red verification state.** Repo-wide formatting defects prevented the normal Go lane from reaching the new package, so a focused workflow was added rather than misreporting that as a model-runtime failure.
- [x] **Step 3: Implement the runtime service** with request size limits, finite-number validation, backend URL validation, model metadata, and existing SCOS auth/control-plane middleware.
- [x] **Step 4: Add a minimal runtime container** built independently from the shared fabric image.
- [x] **Step 5: Run focused formatting, vet, and tests.** Focused gofmt, vet and unit tests reached green during implementation; re-run after final deployment integration before merge.

### Task 2: Correct memory embedding model-space handling

**Files:**
- Modify: `workers/embeddings/tests/test_worker.py`
- Modify: `workers/embeddings/src/scos_embeddings/worker.py`
- Modify: `workers/embeddings/Dockerfile`

**Interfaces:**
- Consumes `SCOS_MODEL_RUNTIME_URL`, internal service credentials, `SCOS_EMBED_MODEL`, `SCOS_EMBED_MODEL_REVISION`, and `SCOS_EMBED_DIMENSIONS`.
- Produces only embeddings matching the active model/revision/dimension tuple for each memory version.

- [x] **Step 1: Write tests** proving source-content hashes, model/revision/dimension-aware backlog selection, and atomic stale-space replacement.
- [x] **Step 2: Implement model-space-aware backlog selection** so an old BGE-small vector does not satisfy a BGE-M3 backfill.
- [x] **Step 3: Correct `chunk_content_hash`** to SHA-256 of the exact source content.
- [x] **Step 4: Add model-runtime inference** with internal bearer/control-plane credentials, strict returned-model metadata validation, and local FastEmbed fallback when runtime is unset.
- [x] **Step 5: Preserve legacy base defaults** until the production deployment explicitly enables BGE-M3.
- [x] **Step 6: Upgrade FastEmbed constraint** to `>=0.8,<1` for BGE-M3 local fallback support.
- [x] **Step 7: Run focused Python worker tests.** They reached green after fixing the 1024-dimensional fixtures; re-run after final deployment integration.

### Task 3: Hosted Linode model deployment

**Files:**
- Modify: `deploy/compose/docker-compose.models.yml`
- Modify: `deploy/scripts/auto-deploy.sh`
- Modify: `.env.example`
- Modify: `docs/model-runtime.md`

**Interfaces:**
- Keeps `model-runtime` and local TEI backends internal to Docker networking.
- Leaves Nginx and public `cloud-control-plane` routes unchanged.
- Makes the real auto-deployer include the model runtime components.

- [x] **Step 1: Add the model-runtime Compose overlay** with internal TLS and private TEI services.
- [x] **Step 2: Change the default TEI image to x86_64 CPU** because production SCOS runs on Linode, not ARM64/Pi.
- [ ] **Step 3: Make the real auto-deploy command include the model overlay** so merged code is actually deployed from `/opt/scos-memory-fabric`.
- [ ] **Step 4: Add production environment variables** for model-runtime URLs, model identities/revisions/dimensions and optional Compose model profiles.
- [ ] **Step 5: Rewrite deployment documentation** around Linode/Nginx/API-key ingress; remove Pi/Tailscale instructions.
- [ ] **Step 6: Verify no model service publishes a public port** and no Nginx change is required.
- [ ] **Step 7: Validate Compose syntax** for the exact production Compose invocation.

### Task 4: Query-aware reranking in memory retrieval

**Files:**
- Modify: `services/retrieval-orchestrator/cmd/server/main_test.go`
- Modify: `services/retrieval-orchestrator/cmd/server/main.go`

**Interfaces:**
- Consumes internal `SCOS_MODEL_RUNTIME_URL` and `/v1/rerank`.
- Produces query-aware ordering while retaining RRF + governance fallback.

- [ ] **Step 1: Add a failing test** showing two RRF-close candidates are reordered by a runtime reranker response.
- [ ] **Step 2: Add a failing test** proving reranker outage preserves RRF ordering rather than failing retrieval.
- [ ] **Step 3: Remove the fake semantic fallback** so unavailable vector inference contributes zero semantic candidates.
- [ ] **Step 4: Preserve raw semantic similarity separately from RRF contribution** instead of overwriting the semantic score field.
- [ ] **Step 5: Restrict vector retrieval to the active model/revision/dimension space.**
- [ ] **Step 6: Call `/v1/rerank` on a bounded top candidate window** before authority/confidence weighting.
- [ ] **Step 7: Run focused retrieval tests.**

### Task 5: BGE-Code dense retrieval

**Files:**
- Modify the durable `services/code-intelligence/cmd/server/*` v2 retrieval path and tests after locating the exact persistence/query implementation.
- Add a migration only if durable fixed-space code vectors require new storage.

**Interfaces:**
- Consumes internal `POST /v1/embed/code`.
- Produces dense code candidates fused with exact, FTS, SCIP/symbol, and graph candidates.

- [ ] **Step 1: Locate the durable `/v2/code/*` query path and write a fixture** where lexical search misses a semantically matching function.
- [ ] **Step 2: Add fixed-space code vector persistence** keyed by snapshot/symbol version plus model/revision/dimensions.
- [ ] **Step 3: Backfill BGE-Code vectors idempotently.**
- [ ] **Step 4: Add dense candidate retrieval and fusion** without weakening exact/SCIP precedence.
- [ ] **Step 5: Run durable-store and tenant-isolation tests.**

### Task 6: Visual retrieval extension

**Files:**
- Separate follow-up service/migration after text/code retrieval benchmarks are green.

**Interfaces:**
- Will consume BGE-VL-large through a dedicated multimodal backend rather than forcing image inputs through text-only TEI endpoints.

- [ ] **Step 1: Keep BGE-VL out of the initial deployment** until an image-capable backend contract is defined and benchmarked.
- [ ] **Step 2: Add the visual lane only after Tasks 1-5 are green.**

### Task 7: Verification and release gate

**Files:**
- Create: `.github/workflows/model-runtime.yml`
- Update: PR #26 description after verification.

**Interfaces:**
- Produces independent evidence for the new subsystem while existing repository-wide CI defects remain visible.

- [x] **Step 1: Add focused CI** for model-runtime Go checks and embedding-worker tests.
- [x] **Step 2: Add model-runtime container build** to focused CI.
- [ ] **Step 3: Re-run focused CI on the final deployment/retrieval integration commit.**
- [ ] **Step 4: Record remaining pre-existing global CI blockers separately.**
- [ ] **Step 5: Keep PR #26 draft until focused CI is green and Tasks 1-4 are integrated.**
