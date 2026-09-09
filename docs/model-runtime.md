# SCOS Model Runtime

SCOS is hosted on the Linode VPS. The public SCOS API remains behind Nginx and the existing `cloud-control-plane` API-key boundary. The model runtime is an internal service and is not a new public API.

## Production request path

External traffic keeps the existing path:

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

The model path currently implemented is:

```text
memory query / memory backfill
  -> embeddings-worker /v1/embed
  -> model-runtime /v1/embed/text
  -> BGE-M3 backend
  -> pgvector memory retrieval
```

The runtime also exposes governed contracts for the next P4 consumers:

```text
retrieval-orchestrator -> model-runtime /v1/rerank -> BGE reranker
code-intelligence      -> model-runtime /v1/embed/code -> BGE-Code
```

Those two consumers are not claimed as complete by this merge. The runtime contract exists so they can be added without exposing raw model servers or changing the public SCOS API.

Neither `model-runtime` nor the raw model servers belongs in Nginx. Customer API keys never get forwarded to them.

## Models

| Role | Default model | Output |
| --- | --- | --- |
| text | `BAAI/bge-m3` | 1024-dimensional dense vector |
| rerank | `BAAI/bge-reranker-v2-m3` | relevance score per candidate |
| code | `BAAI/bge-code-v1` | configured 1536-dimensional dense vector |

`BAAI/BGE-VL-large` remains separate because it needs a multimodal image/text contract rather than the text-only TEI path.

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

The backend URLs are private configuration:

```text
SCOS_MODEL_TEXT_URL
SCOS_MODEL_RERANK_URL
SCOS_MODEL_CODE_URL
```

For local TEI services, Compose passes the same `SCOS_MODEL_*_MODEL` and `SCOS_MODEL_*_REVISION` values to both TEI and model-runtime. This prevents model-runtime from advertising a revision that the locally managed backend was never instructed to load.

For externally hosted backends, the operator remains responsible for configuring the URL to the model/revision declared in SCOS. Raw backend access should remain on a private governed network.

## Safe production activation

The model overlay is intentionally **opt-in**. Merging model-runtime code into `main` must not unexpectedly cold-load multi-GB inference models on an already-running host.

In the live `.env`:

```bash
SCOS_MODEL_RUNTIME_ENABLED=true
SCOS_MODEL_PROFILES=text-model
```

`deploy/scripts/auto-deploy.sh` then automatically adds:

```text
deploy/compose/docker-compose.models.yml
```

and waits for `model-runtime` to become healthy before declaring the deployment successful.

Without `SCOS_MODEL_RUNTIME_ENABLED=true`, the existing base deployment remains on the current local bge-small embedding worker path.

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

Do not enable every model just because YAML has developed ambitions. Measure resident memory and inference latency on the actual host first.

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

Its SCOS-facing listener is HTTPS. `embeddings-worker` trusts the model-runtime certificate through `SCOS_MODEL_RUNTIME_CA_FILE` and sends both internal authentication headers.

This is separate from the `scos_live_...` customer key system. Customer credentials terminate at `cloud-control-plane`.

## Memory embedding migration

Base Compose stays compatible with the current vector space:

```text
model:      BAAI/bge-small-en-v1.5
revision:   local
dimensions: 384
```

When the model overlay is enabled, it changes the active text space to:

```text
model:      BAAI/bge-m3
revision:   SCOS_MODEL_TEXT_REVISION
dimensions: 1024
```

The current retrieval SQL predates per-model-space predicates. To make the migration safe with that current repository code, `embeddings-worker` first deletes `memory_embeddings` rows outside the one active model/revision/dimension space, then backfills missing memory versions. This prevents a 1024-dimensional BGE-M3 query from being compared to a remaining 384-dimensional BGE-small row during rollout.

This is deliberately a compatibility bridge. The P4 retrieval work should still add explicit active-space predicates so multiple historical/experimental spaces can coexist later.

`chunk_content_hash` is SHA-256 of the source content rather than a hash of generated vector values.

Production should pin each model to an exact Hugging Face revision after validation rather than leaving `main` mutable indefinitely.

## Current implementation state

Implemented by this merge:

```text
memory version
  -> embeddings-worker
  -> model-runtime /v1/embed/text
  -> configured BGE-M3 backend
  -> validated 1024-dimensional vector
  -> PostgreSQL memory_embeddings
  -> existing retrieval-orchestrator pgvector path
```

Also present as governed runtime contracts:

```text
/v1/rerank
/v1/embed/code
```

Still separate P4 work:

- query-aware BGE reranker consumption in `retrieval-orchestrator`;
- explicit model-space predicates in retrieval SQL instead of the single-active-space compatibility purge;
- removal of the current semantic fallback behavior;
- durable BGE-Code vector persistence and hybrid code fusion;
- BGE-VL multimodal indexing.

## Verification

Focused CI lives in `.github/workflows/model-runtime.yml` and checks:

```text
model-runtime gofmt
go vet
model-runtime unit tests
model-runtime container build
embedding-worker compile
embedding-worker unit tests
auto-deploy shell syntax
active text-model Compose configuration
```

Global repository CI is a separate signal and may include unrelated pre-existing failures; model-runtime merge decisions must use fresh focused results plus the relevant repository checks, not a stale run from an older branch.
