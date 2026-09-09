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

The model path is internal:

```text
embeddings-worker
  -> model-runtime /v1/embed/text
  -> BGE-M3 backend

retrieval-orchestrator
  -> model-runtime /v1/rerank
  -> BGE reranker backend

code-intelligence
  -> model-runtime /v1/embed/code
  -> BGE-Code backend
```

Neither `model-runtime` nor the raw model servers belong in Nginx. Customer API keys never get forwarded to them.

## Models

| Role | Default model | Output |
| --- | --- | --- |
| text | `BAAI/bge-m3` | 1024-dimensional dense vector |
| rerank | `BAAI/bge-reranker-v2-m3` | relevance score per candidate |
| code | `BAAI/bge-code-v1` | expected 1536-dimensional dense vector |

`BAAI/BGE-VL-large` is intentionally separate because it needs a multimodal image/text contract rather than the text-only TEI API.

## Internal SCOS API

```text
POST /v1/embed/text
POST /v1/embed/code
POST /v1/rerank
GET  /health/live
GET  /health/ready
GET  /health/version
```

`model-runtime` validates model identity, revision, vector dimensions, finite numeric values and reranker indexes before returning inference results to SCOS.

## Runtime backend

The reference backend is Hugging Face Text Embeddings Inference 1.9. The production Linode is x86_64, so the default local image is:

```text
ghcr.io/huggingface/text-embeddings-inference:cpu-1.9
```

The model backend URLs are configuration, not public routes:

```text
SCOS_MODEL_TEXT_URL
SCOS_MODEL_RERANK_URL
SCOS_MODEL_CODE_URL
```

Local Compose defaults resolve these to the private services `bge-m3`, `bge-reranker`, and `bge-code`. A different inference host can be supplied through those variables without changing any caller-facing SCOS URL.

## Production Compose

The model definitions live in:

```text
deploy/compose/docker-compose.models.yml
```

The real deployment script runs from `/opt/scos-memory-fabric` and must combine it with the base Compose file:

```bash
docker compose \
  --env-file .env \
  -f deploy/compose/docker-compose.yml \
  -f deploy/compose/docker-compose.models.yml \
  up -d
```

The production auto-deployer owns this invocation. Do not start a second hand-managed copy of the same services beside it.

## Local model profiles

`BGE-M3` is the baseline text model. The reranker and code model can be enabled independently when their consumers are active and the Linode has enough resources.

The profile names are:

```text
text-model
rerank-model
code-model
```

For a local BGE-M3 backend:

```bash
COMPOSE_PROFILES=text-model docker compose \
  --env-file .env \
  -f deploy/compose/docker-compose.yml \
  -f deploy/compose/docker-compose.models.yml \
  up -d bge-m3 model-runtime embeddings-worker retrieval-orchestrator
```

To run all three model backends locally:

```bash
COMPOSE_PROFILES=text-model,rerank-model,code-model docker compose \
  --env-file .env \
  -f deploy/compose/docker-compose.yml \
  -f deploy/compose/docker-compose.models.yml \
  up -d
```

Do not enable all three merely because the YAML permits it. Their actual resident memory and inference latency must be measured on the Linode first.

## Public ingress is unchanged

The current hosted stack already distinguishes the externally reachable gateway from Docker-internal service addresses. `cloud-control-plane` requires `SCOS_PUBLIC_GATEWAY_URL`, validates long-lived cloud API keys on `/mcp`, and proxies downstream using signed internal claims.

This model-runtime change therefore requires:

```text
Nginx changes:        none
new public URL:       none
new customer API key: none
public model port:    none
```

Raw TEI services use Compose `expose`, not public `ports`.

## Internal TLS and authentication

`model-runtime` uses the same two internal SCOS credentials used by governed services:

```text
SCOS_INTERNAL_SERVICE_TOKEN
SCOS_CONTROL_PLANE_TOKEN
```

Its SCOS-facing listener is HTTPS. The embeddings worker trusts the model-runtime certificate through `SCOS_MODEL_RUNTIME_CA_FILE`.

This is separate from the `scos_live_...` customer key system. Customer credentials terminate at `cloud-control-plane` and must not be reused as model-server credentials.

## Memory embedding migration

When BGE-M3 is enabled, the active text space is:

```text
model:      BAAI/bge-m3
revision:   configured revision
dimensions: 1024
```

The embeddings worker treats an old BGE-small vector as stale. It re-embeds the memory version into the current model/revision/dimension space and removes stale vectors for that version in the same database transaction.

`chunk_content_hash` is SHA-256 of the source content rather than a hash of the generated embedding.

Production should pin each model to an exact Hugging Face revision instead of leaving `main` mutable indefinitely.

## Current implementation state

Working:

```text
memory version
  -> embeddings-worker
  -> model-runtime /v1/embed/text
  -> configured BGE-M3 backend
  -> validated 1024-dimensional vector
  -> PostgreSQL memory_embeddings
```

Runtime endpoints also exist for:

```text
/v1/rerank
/v1/embed/code
```

The retrieval orchestrator still needs its query-aware reranker integration and active-space vector filtering completed. Code intelligence still needs durable BGE-Code vector persistence/fusion. Those are explicit remaining tasks in `docs/superpowers/plans/2026-09-09-model-runtime-bge.md`.

## Verification

Focused CI lives in `.github/workflows/model-runtime.yml` and checks:

```text
model-runtime gofmt
go vet
model-runtime unit tests
model-runtime container build
embedding-worker compile
embedding-worker unit tests
```

Before PR #26 leaves draft status, run the focused workflow on the final deployment/retrieval integration commit and validate the exact production Compose invocation used by `deploy/scripts/auto-deploy.sh`.
