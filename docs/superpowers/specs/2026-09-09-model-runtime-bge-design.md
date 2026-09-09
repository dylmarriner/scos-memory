# SCOS BGE Model Runtime Design

Date: 2026-09-09
Status: approved direction, implementation target
Branch: `feature/model-runtime-bge`

## Objective

Replace the current single-purpose `bge-small-en-v1.5` FastEmbed worker contract with a model-runtime boundary that can serve specialized retrieval models without coupling SCOS services to a particular inference engine.

The first production path must support:

- general memory/document embeddings with `BAAI/bge-m3`;
- query-aware reranking with `BAAI/bge-reranker-v2-m3`;
- code embeddings with `BAAI/bge-code-v1`;
- a clean extension point for `BAAI/BGE-VL-large` visual embeddings;
- remote inference so the Raspberry Pi deployment does not need to host the heavyweight models;
- local CPU inference as an optional development/fallback path where practical.

## Non-goals

This change does not replace PostgreSQL, pgvector, SCOS RLS, memory governance, retrieval RRF, or code-intelligence parsing. It does not add a generic chat-LLM host. It does not silently mix vectors from different models or dimensions.

## Architecture

SCOS services communicate with one stable internal runtime API:

```text
retrieval-orchestrator ─┐
code-intelligence ──────┼──> model-runtime
future document RAG ────┤       ├── text embedding backend
future visual RAG ──────┘       ├── reranker backend
                                ├── code embedding backend
                                └── visual embedding backend
```

`model-runtime` is an HTTP routing/contract service. Inference backends are configurable endpoints. Heavy models can run on another x86/GPU node while the SCOS control/data plane remains on the Pi.

Default model roles:

| Role | Model | Dimensions / output |
| --- | --- | --- |
| `text` | `BAAI/bge-m3` | 1024 dense vector |
| `rerank` | `BAAI/bge-reranker-v2-m3` | relevance score |
| `code` | `BAAI/bge-code-v1` | model-specific dense vector, expected 1536 |
| `vision` | `BAAI/BGE-VL-large` | 768 shared image/text vector |

The runtime must return model identity, revision, dimensions, and role with every embedding response.

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

Same response shape, using the configured code backend.

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

The first backend protocol is Hugging Face Text Embeddings Inference compatible HTTP for text/code embeddings and reranking where supported. The SCOS runtime translates the stable SCOS API into backend-specific requests.

Backend URLs are configured independently:

- `SCOS_MODEL_TEXT_URL`
- `SCOS_MODEL_RERANK_URL`
- `SCOS_MODEL_CODE_URL`
- `SCOS_MODEL_VISION_URL`

The model runtime never assumes those services run on the same host.

## Embedding-space correctness

The existing `memory_embeddings` table permits multiple model/revision/dimension combinations but the worker currently treats any embedding for a memory version as sufficient. That must be corrected.

A memory embedding is current only when all of these match the configured active text space:

- `provider`;
- `model`;
- `model_revision`;
- `dimensions`;
- `chunk_number`.

Retrieval must select only embeddings from the active text space. It must never compare a BGE-M3 query vector with a BGE-small or BGE-Code vector.

The existing `chunk_content_hash` must become a hash of the exact source chunk bytes, not a hash of the generated embedding.

## Retrieval changes

`retrieval-orchestrator` will use `SCOS_MODEL_RUNTIME_URL` rather than directly knowing about FastEmbed.

Retrieval behavior:

1. exact search;
2. PostgreSQL FTS;
3. text embedding via `/v1/embed/text`;
4. pgvector search restricted to the active model/revision/dimensions;
5. relation/session expansion;
6. RRF fusion;
7. rerank the top candidate window through `/v1/rerank` when configured;
8. apply authority/confidence governance weighting after relevance reranking;
9. allocate token budget and assemble context.

If embedding is unavailable, vector retrieval contributes zero candidates. SCOS must not fabricate a semantic result set from unrelated high-confidence memories.

If reranking is unavailable, RRF + governance weighting remains the explicit degraded path.

## Persistence changes

Add an `embedding_spaces` table containing:

- stable role/name;
- provider;
- model;
- model revision;
- dimensions;
- modality;
- distance metric;
- active flag;
- timestamps.

`memory_embeddings` gains an `embedding_space_id` reference. A migration backfills the existing BGE-small rows into a legacy text space before activating BGE-M3.

Indexes must be scoped so vector search can efficiently filter to the active embedding space. HNSW creation is allowed only for a fixed-dimensional representation. If pgvector schema constraints make one mixed-dimension HNSW index impossible, create role/model-specific vector storage rather than pretending heterogeneous dimensions are one search space.

## Deployment

The default Pi Compose stack includes `model-runtime` but does not start heavyweight inference containers by default.

Two deployment modes are supported:

### Remote inference, recommended for the Pi

```text
Pi 5
  SCOS services
  Postgres/pgvector
  model-runtime
        │ Tailscale/private network
        ▼
x86/GPU node
  BGE-M3 backend
  reranker backend
  BGE-Code backend
  optional BGE-VL backend
```

### Local development

Optional Compose profiles can run supported local inference backends when the host has sufficient RAM/GPU. These profiles are not part of the Pi baseline memory budget.

## Security

- `model-runtime` uses the existing SCOS internal service authentication/TLS conventions for SCOS-facing endpoints.
- Backend inference endpoints remain private and are never exposed as public unauthenticated ports by the reference deployment.
- Request bodies have explicit size limits.
- Runtime responses are validated for dimensions, finite numeric values, model identity, and candidate indexes.
- Backend failures degrade honestly and are observable; they never silently substitute a different model space.

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
10. tenant/RLS behavior remaining unchanged.

Integration tests should use deterministic local HTTP test servers, not live Hugging Face endpoints.

## Rollout

1. Add the runtime service and tests without changing existing retrieval behavior.
2. Add embedding-space schema and migrate legacy vectors.
3. Point retrieval to model-runtime text embedding and make BGE-M3 the configured active text space.
4. Add reranking to the retrieval pipeline.
5. Add BGE-Code integration to durable code retrieval.
6. Add optional BGE-VL visual artifact indexing as a separate follow-up once text/code retrieval is benchmarked.

## Acceptance criteria

- No cross-model vector comparison is possible on the normal retrieval path.
- Switching model revision causes missing-current-space embeddings to be regenerated.
- BGE-M3 text embedding and BGE reranking work through one SCOS-owned HTTP contract.
- The Pi can operate with inference hosted remotely.
- Disabling the inference backends leaves exact/FTS/relation/session retrieval functional and explicitly marks semantic/rerank capability unavailable.
- Existing memory governance and tenant isolation remain intact.
- CI covers the runtime contract and embedding-space regression cases.
