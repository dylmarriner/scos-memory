# SCOS BGE Model Runtime Design

Date: 2026-09-09
Status: approved direction, implementation target
Branch: `feature/model-runtime-bge`

## Objective

Replace the single-purpose `bge-small-en-v1.5` FastEmbed path with a governed model-runtime boundary that supports specialized retrieval models without coupling SCOS services to a particular inference engine.

The first production path supports:

- general memory/document embeddings with `BAAI/bge-m3`;
- query-aware reranking with `BAAI/bge-reranker-v2-m3`;
- code embeddings with `BAAI/bge-code-v1`;
- a clean extension point for `BAAI/BGE-VL-large` visual embeddings;
- local inference on the hosted SCOS server when resources permit;
- optional remote inference through explicitly configured backend URLs without changing the SCOS-facing API.

## Production topology

SCOS is hosted on the Linode VPS. External clients do not connect to internal services directly.

The existing public path is:

```text
Internet client / agent
        │
        │ HTTPS + scos_live_... API key
        ▼
      Nginx
        │
        ▼
cloud-control-plane
        │
        │ validate cloud_api_keys
        │ replace customer key with short-lived signed claims
        ▼
   mcp-gateway
        │
        ├── memory/retrieval services
        ├── code-intelligence
        └── other governed SCOS services
```

The Nginx configuration is deployment-host configuration and is not owned by this model-runtime change. No new public Nginx route is required.

The model runtime is an internal service only:

```text
embeddings-worker ───────┐
retrieval-orchestrator ──┼──> model-runtime
code-intelligence ───────┤       ├── BGE-M3 backend
future document RAG ─────┤       ├── BGE reranker backend
future visual RAG ───────┘       ├── BGE-Code backend
                                └── future visual backend
```

Customer API keys terminate at `cloud-control-plane`. `model-runtime` never accepts or stores `scos_live_...` customer credentials. It uses the existing SCOS service-to-service bearer, control-plane token, and internal TLS conventions.

## Non-goals

This change does not replace PostgreSQL, pgvector, SCOS RLS, memory governance, retrieval RRF, code-intelligence parsing, Nginx, or the public API-key boundary. It does not add a generic chat-LLM host. It does not expose raw inference servers publicly. It does not silently mix vectors from different models or dimensions.

## Model roles

| Role | Model | Dimensions / output |
| --- | --- | --- |
| `text` | `BAAI/bge-m3` | 1024 dense vector |
| `rerank` | `BAAI/bge-reranker-v2-m3` | relevance score |
| `code` | `BAAI/bge-code-v1` | expected 1536-dimensional dense vector |
| `vision` | `BAAI/BGE-VL-large` | 768 shared image/text vector |

The runtime returns model identity, revision, dimensions, and role with every embedding response.

## API contract

### `POST /v1/embed/text`

Request:

```json
{"input":"text to embed"}
```

Response:

```json
{
  "embedding":[0.1,0.2],
  "model":"BAAI/bge-m3",
  "model_revision":"...",
  "dimensions":1024,
  "role":"text"
}
```

### `POST /v1/embed/code`

Same response shape using the configured code backend.

### `POST /v1/rerank`

Request:

```json
{
  "query":"How does SCOS recover interrupted turns?",
  "documents":["candidate one","candidate two"],
  "top_n":20
}
```

Response:

```json
{
  "model":"BAAI/bge-reranker-v2-m3",
  "model_revision":"...",
  "results":[
    {"index":1,"score":0.91},
    {"index":0,"score":0.47}
  ]
}
```

### Health

`GET /health/live` reports process liveness.

`GET /health/ready` checks configured required backends and fails closed when a required role is unavailable.

## Backend protocol

The first backend protocol is Hugging Face Text Embeddings Inference compatible HTTP for text/code embeddings and reranking where supported. Backend URLs are configured independently:

- `SCOS_MODEL_TEXT_URL`
- `SCOS_MODEL_RERANK_URL`
- `SCOS_MODEL_CODE_URL`
- future `SCOS_MODEL_VISION_URL`

The reference deployment keeps local TEI services on the Docker network with `expose`, not public `ports`. If a backend is hosted elsewhere, its transport/authentication is an operator deployment concern and must not weaken the public SCOS API-key boundary.

## Embedding-space correctness

A memory embedding is current only when these match the configured active text space:

- provider;
- model;
- model revision;
- dimensions;
- chunk number.

Retrieval must select only embeddings from the active text space. It must never compare a BGE-M3 query vector with BGE-small or BGE-Code vectors.

`chunk_content_hash` is SHA-256 of the exact source content, independent of generated vector values.

## Retrieval changes

The target retrieval path is:

1. exact search;
2. PostgreSQL FTS;
3. BGE-M3 text embedding;
4. pgvector search restricted to active model/revision/dimensions;
5. relation/session expansion;
6. RRF fusion;
7. query-aware reranking through `/v1/rerank` when configured;
8. authority/confidence governance weighting after relevance reranking;
9. token-budget allocation and context assembly.

If embedding is unavailable, vector retrieval contributes zero candidates. SCOS must not fabricate a semantic result set from unrelated high-confidence memories.

If reranking is unavailable, RRF + governance weighting remains the explicit degraded path.

## Persistence

The worker must enforce model/revision/dimension-aware re-embedding. A future schema migration may formalize named embedding spaces and fixed-dimension indexes, but the normal path must already prevent cross-space comparison before that migration is considered complete.

## Deployment requirements

- SCOS remains hosted on the Linode VPS.
- Existing public Nginx and `cloud-control-plane` API-key ingress remain unchanged.
- `model-runtime` is internal and is not added to Nginx.
- Raw BGE inference servers are internal and are not added to Nginx.
- The existing auto-deploy path must include whatever Compose files are required for the model runtime, otherwise the feature is not considered deployed.
- The production model backend choice must be configurable because the Linode's available CPU/RAM/GPU resources determine whether BGE models run locally or on a separate inference host.

## Security

- Customer `scos_live_...` keys are validated only at `cloud-control-plane` and are replaced by short-lived signed internal claims before requests enter the service mesh.
- `model-runtime` uses existing SCOS internal authentication and control-plane protection.
- Internal TLS remains fail-closed.
- Backend inference endpoints remain private.
- Request bodies have explicit size limits.
- Runtime responses validate dimensions, finite numeric values, model identity, and candidate indexes.
- Backend failures degrade honestly and never substitute a different model space.

## Testing

Tests are required for:

1. routing each role to the configured backend;
2. rejecting empty/oversized requests;
3. validating backend response model/dimensions;
4. reranker ordering and index validation;
5. embedding-space model/revision filtering;
6. re-embedding when active model/revision changes;
7. source-content hash correctness;
8. vector-backend failure producing no semantic candidates;
9. retrieval fallback when reranker is unavailable;
10. tenant/RLS behavior remaining unchanged;
11. model-runtime remaining absent from the public ingress surface.

Integration tests use deterministic local HTTP test servers, not live Hugging Face endpoints.

## Rollout

1. Add runtime service and contract tests.
2. Correct memory embedding model-space handling.
3. Wire the hosted Linode deployment and auto-deploy path without altering Nginx/public routes.
4. Make BGE-M3 the active text space.
5. Add query-aware reranking.
6. Add BGE-Code durable retrieval.
7. Add BGE-VL visual artifact indexing separately after text/code benchmarks are green.

## Acceptance criteria

- No cross-model vector comparison occurs on the normal retrieval path.
- Switching model revision causes stale/missing embeddings to regenerate.
- BGE-M3 text embedding and BGE reranking work through one SCOS-owned internal HTTP contract.
- No model endpoint is exposed through the public Nginx/API-key surface.
- Disabling inference backends leaves exact/FTS/relation/session retrieval functional.
- Existing cloud API-key, tenant isolation, RLS, and governance behavior remain intact.
- The real `/opt/scos-memory-fabric` deployment path starts the required model-runtime components after merge.
- Focused CI covers runtime and embedding-space regression cases.
