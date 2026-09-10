# SCOS Model Runtime

SCOS is hosted on the Linode VPS documented in `docs/runbooks/production-access.md`. The public SCOS API remains behind Nginx and the existing `cloud-control-plane` API-key boundary. `model-runtime` is internal retrieval infrastructure, not a public model API.

## Production request path

External traffic keeps the existing governed path:

```text
client / agent
  -> HTTPS public SCOS URL
  -> Nginx
  -> cloud-control-plane
       -> validate scos_live_... API key
       -> issue short-lived signed internal claims
  -> mcp-gateway
  -> governed SCOS services
```

DeepSeek remains SCOS's existing generative/reasoning layer. BGE is the specialist retrieval layer underneath it.

```text
memory query
  -> retrieval-orchestrator
       -> model-runtime /v1/embed/text -> BGE-M3
       -> exact / FTS / pgvector / relation / session lanes
       -> RRF
       -> model-runtime /v1/rerank -> BGE reranker
       -> authority/confidence governance weighting

memory backfill
  -> embeddings-worker
       -> model-runtime /v1/embed/text -> BGE-M3
       -> explicit text embedding space
       -> memory_embeddings

code index/query
  -> code-intelligence
       -> model-runtime /v1/embed/code -> BGE-Code
       -> durable code_embeddings on immutable symbol versions
       -> exact / lexical / dense / structural-SCIP / graph-PageRank signals
       -> RRF fusion
```

Neither `model-runtime` nor the raw TEI model servers belongs in Nginx. Customer API keys never get forwarded to them.

## Models

| Role | Default model | Output |
| --- | --- | --- |
| text | `BAAI/bge-m3` | 1024-dimensional dense vector |
| rerank | `BAAI/bge-reranker-v2-m3` | relevance score per candidate |
| code | `BAAI/bge-code-v1` | configured 1536-dimensional dense vector |

`BAAI/BGE-VL-large` remains later multimodal work because it needs a separate visual/image contract and embedding space.

## Internal SCOS API

```text
POST /v1/embed/text
POST /v1/embed/code
POST /v1/rerank
GET  /health/live
GET  /health/ready
GET  /health/version
```

`model-runtime` validates configured role, revision metadata, vector dimensions, finite numeric values, backend URL shape, request size and reranker indexes. Explicit malformed `SCOS_MODEL_*_REQUIRED` values fail configuration instead of silently disabling readiness checks.

## Runtime backend

The reference backend is Hugging Face Text Embeddings Inference 1.9. The hosted x86_64 CPU baseline is:

```text
ghcr.io/huggingface/text-embeddings-inference:cpu-1.9
```

Backend URLs are private configuration:

```text
SCOS_MODEL_TEXT_URL
SCOS_MODEL_RERANK_URL
SCOS_MODEL_CODE_URL
```

For local TEI services, Compose passes the same `SCOS_MODEL_*_MODEL` and `SCOS_MODEL_*_REVISION` values to both TEI and `model-runtime`. This prevents the runtime from advertising a revision that the managed backend was never instructed to load.

For externally hosted backends, the operator remains responsible for configuring each URL to the model/revision declared in SCOS. Raw backend access should remain private and governed.

## Explicit embedding spaces

P4 defines embedding-space identity as:

```text
role + provider + model + revision + dimensions + generation
```

Migration `000036_embedding_spaces_and_code_embeddings.sql` creates the shared `embedding_spaces` registry, backfills legacy memory vectors into explicit generation-1 spaces, and adds durable tenant-owned `code_embeddings` tied to immutable `code_symbol_versions`.

Memory retrieval resolves the exact configured text-space ID before any pgvector distance operation and filters `memory_embeddings` by that ID. Historical BGE-small and experimental spaces can therefore coexist without participating in the same distance calculation.

The embeddings worker no longer purges other vector spaces. It resolves/creates the active text space and idempotently writes `(memory_version_id, embedding_space_id, chunk_number)` projections while retaining historical spaces.

Code retrieval uses the same space abstraction for BGE-Code. Dense code queries require the active code space and active repository snapshot, so old snapshots and mismatched model spaces cannot leak into current results.

`chunk_content_hash` is SHA-256 of the exact source/document content used for a projection, not a hash of generated vector values.

## Retrieval degradation rules

Model failure never creates fake semantic evidence.

```text
text embedding unavailable -> zero memory semantic candidates
pgvector lane unavailable  -> zero memory semantic candidates
reranker unavailable       -> RRF + governance fallback
BGE-Code unavailable       -> lexical/structural code path continues with degraded metadata
```

Raw scoring signals remain distinct. Memory retrieval preserves raw lexical score, raw cosine similarity, RRF score, reranker score and final governance-weighted score. Code retrieval preserves lexical, dense, structural/graph and fused scores independently.

## Code retrieval and agent exposure

Durable `/v2/code/query` keeps exact path/FQN matches as precision anchors. Non-exact searches combine lexical, BGE-Code dense, persisted structural/SCIP edges and bounded personalized PageRank signal.

`scos_code_query` retains legacy v1 compatibility while supporting the durable v2 path when durable repository context is supplied. `scos_retrieve` accepts optional durable `repository_id` + `branch`; deterministic `code`/`mixed` plans can then retrieve code evidence independently of memory namespaces. Selected immutable code-symbol-version IDs are recorded with the retrieval run rather than being disguised as memory IDs.

## Safe production activation

The model overlay is intentionally **opt-in**. Merging retrieval support into `main` must not unexpectedly cold-load multi-GB inference models on the existing Linode.

Production activation uses the live `.env`:

```bash
SCOS_MODEL_RUNTIME_ENABLED=true
SCOS_MODEL_PROFILES=text-model
```

or, only after host-capacity validation:

```bash
SCOS_MODEL_RUNTIME_ENABLED=true
SCOS_MODEL_PROFILES=text-model,rerank-model,code-model
```

`deploy/scripts/auto-deploy.sh` adds `deploy/compose/docker-compose.models.yml` when enabled. On the first model-enabled deployment it initializes the private model-runtime TLS certificate before force-recreating retrieval/code clients, then waits for `model-runtime` readiness.

Without `SCOS_MODEL_RUNTIME_ENABLED=true`, base deployment behavior remains unchanged.

## Local model profiles

Profiles are:

```text
text-model
rerank-model
code-model
```

For BGE-M3 only:

```bash
COMPOSE_PROFILES=text-model docker compose \
  --env-file .env \
  -f deploy/compose/docker-compose.yml \
  -f deploy/compose/docker-compose.models.yml \
  up -d
```

For all three local backends:

```bash
COMPOSE_PROFILES=text-model,rerank-model,code-model docker compose \
  --env-file .env \
  -f deploy/compose/docker-compose.yml \
  -f deploy/compose/docker-compose.models.yml \
  up -d
```

Do not enable every model merely because Compose permits it. Measure resident memory, CPU saturation and query latency on the actual Linode first.

## Public ingress is unchanged

```text
Nginx changes:        none
new public URL:       none
new customer API key: none
public model port:    none
```

Raw TEI services and `model-runtime` use Compose-internal networking only.

## Internal TLS and authentication

`model-runtime` uses:

```text
SCOS_INTERNAL_SERVICE_TOKEN
SCOS_CONTROL_PLANE_TOKEN
```

Its SCOS-facing listener is HTTPS. `embeddings-worker`, `retrieval-orchestrator` and `code-intelligence` trust its certificate through `SCOS_MODEL_RUNTIME_CA_FILE` and use governed internal credentials.

This is separate from the `scos_live_...` customer-key system. Customer credentials terminate at `cloud-control-plane`.

## Production revision policy

Development defaults may still use `main`, but production must pin each validated Hugging Face model to an immutable revision before activation. Changing a model/revision/dimension/generation creates a new explicit embedding space rather than silently reinterpreting old vectors.

## Verification

Focused source checks live in `.github/workflows/model-runtime.yml` and `.github/workflows/p4-hybrid-retrieval.yml`. They cover model-runtime, retrieval-orchestrator, code-intelligence, embeddings-worker, migration contracts, the P0/P4 smoke fixture, auto-deploy shell syntax and the merged Compose overlay with text/rerank/code profiles.

Local P4 verification additionally uses a fresh PostgreSQL 17 + pgvector database so migrations, tenant RLS, active-snapshot filtering and retrieval SQL are exercised against real storage.

GitHub Actions runner launch failures are infrastructure failures when jobs contain no executed steps; they must not be reported as product test failures or as successful verification.

## Current source status versus live deployment

The P4 source implementation contains explicit memory/code spaces, honest degradation, BGE reranking, durable BGE-Code hybrid retrieval, structural/graph fusion and deterministic repo-aware query planning.

That source status is distinct from live production activation. The Linode must still be inspected for current checkout, environment flags, available memory/CPU, pinned model revisions and actual model-runtime readiness before claiming the BGE profiles are live in production.
