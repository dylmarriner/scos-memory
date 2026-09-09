# SCOS Model Runtime

SCOS uses a small governed `model-runtime` service as the stable internal API for retrieval-model inference. Heavy model servers can run beside SCOS or on another private/Tailscale-connected machine.

The first supported model roles are:

| Role | Default model | Expected output |
| --- | --- | --- |
| text | `BAAI/bge-m3` | 1024-dimensional dense vector |
| rerank | `BAAI/bge-reranker-v2-m3` | relevance score per candidate |
| code | `BAAI/bge-code-v1` | 1536-dimensional dense vector |

`BAAI/BGE-VL-large` is intentionally not started by this overlay. It is a CLIP/custom-code multimodal model and needs an image-capable runtime contract rather than being forced through a text-only TEI endpoint.

## Why there is a runtime boundary

SCOS services must not know whether an embedding was produced by ARM64 CPU, NVIDIA CUDA, or a remote inference host. They call:

```text
POST /v1/embed/text
POST /v1/embed/code
POST /v1/rerank
```

`model-runtime` validates the backend response, model identity and vector dimensions before returning it to SCOS.

The raw inference servers remain private.

## Runtime backend

The reference backend is Hugging Face Text Embeddings Inference 1.9.

Official documentation:

- https://huggingface.co/docs/text-embeddings-inference/quick_tour
- https://huggingface.co/docs/text-embeddings-inference/supported_models
- https://huggingface.co/docs/text-embeddings-inference/cli_arguments

Current TEI 1.9 image families include:

| Host | Image |
| --- | --- |
| x86_64 CPU | `ghcr.io/huggingface/text-embeddings-inference:cpu-1.9` |
| ARM64 CPU | `ghcr.io/huggingface/text-embeddings-inference:cpu-arm64-1.9` |
| Turing / T4 / RTX 2000 | `ghcr.io/huggingface/text-embeddings-inference:turing-1.9` |
| Ampere 8.0 / A100 / A30 | `ghcr.io/huggingface/text-embeddings-inference:1.9` |
| Ampere 8.6 / A10 / A40 | `ghcr.io/huggingface/text-embeddings-inference:86-1.9` |
| Ada / RTX 4000 | `ghcr.io/huggingface/text-embeddings-inference:89-1.9` |
| Hopper / H100 | `ghcr.io/huggingface/text-embeddings-inference:hopper-1.9` |

TEI does not support NVIDIA compute capability below 7.5, including V100.

## Mode 1: Raspberry Pi, local BGE-M3

This is functional but not the preferred production arrangement. BGE-M3 is much larger than the old BGE-small worker model, so expect substantial RAM use and slower indexing.

From the repository root:

```bash
cd deploy/compose

docker compose \
  -f docker-compose.yml \
  -f docker-compose.models.yml \
  --profile text-model \
  up -d bge-m3 model-runtime embeddings-worker retrieval-orchestrator
```

The model overlay uses the ARM64 CPU TEI 1.9 image by default.

Check the runtime:

```bash
docker compose \
  -f docker-compose.yml \
  -f docker-compose.models.yml \
  ps bge-m3 model-runtime embeddings-worker retrieval-orchestrator
```

Then verify health inside the private Compose network:

```bash
docker compose \
  -f docker-compose.yml \
  -f docker-compose.models.yml \
  exec model-runtime wget -q -O - http://127.0.0.1:8103/health/ready
```

A healthy text backend returns:

```json
{"status":"ready"}
```

## Mode 2: Remote x86 or NVIDIA model host

This is the recommended layout for the Pi deployment.

The model host should be reachable only on a private interface such as Tailscale. Do not publish raw TEI endpoints on `0.0.0.0` to the public internet.

Set the model host's Tailscale address once:

```bash
export TS_IP="$(tailscale ip -4 | head -n1)"
mkdir -p "$HOME/scos-models/bge-m3" "$HOME/scos-models/bge-reranker" "$HOME/scos-models/bge-code"
```

### CPU x86_64

```bash
docker run -d --name scos-bge-m3 \
  --restart unless-stopped \
  -p "$TS_IP:8104:80" \
  -v "$HOME/scos-models/bge-m3:/data" \
  ghcr.io/huggingface/text-embeddings-inference:cpu-1.9 \
  --model-id BAAI/bge-m3 \
  --port 80
```

### NVIDIA Ada / RTX 4000 example

The host needs an NVIDIA driver compatible with CUDA 12.2+ and the NVIDIA Container Toolkit.

```bash
docker run -d --name scos-bge-m3 \
  --restart unless-stopped \
  --gpus all \
  -p "$TS_IP:8104:80" \
  -v "$HOME/scos-models/bge-m3:/data" \
  ghcr.io/huggingface/text-embeddings-inference:89-1.9 \
  --model-id BAAI/bge-m3 \
  --port 80
```

Start the reranker on a second private port:

```bash
docker run -d --name scos-bge-reranker \
  --restart unless-stopped \
  --gpus all \
  -p "$TS_IP:8105:80" \
  -v "$HOME/scos-models/bge-reranker:/data" \
  ghcr.io/huggingface/text-embeddings-inference:89-1.9 \
  --model-id BAAI/bge-reranker-v2-m3 \
  --port 80
```

Start the code embedder on a third private port:

```bash
docker run -d --name scos-bge-code \
  --restart unless-stopped \
  --gpus all \
  -p "$TS_IP:8106:80" \
  -v "$HOME/scos-models/bge-code:/data" \
  ghcr.io/huggingface/text-embeddings-inference:89-1.9 \
  --model-id BAAI/bge-code-v1 \
  --port 80
```

For A10/A40 use `86-1.9` instead of `89-1.9`. For T4 use `turing-1.9`.

## Point the Pi at the remote model host

On the SCOS host, export the remote private URLs before starting the overlay:

```bash
export SCOS_MODEL_TEXT_URL="http://100.x.y.z:8104"
export SCOS_MODEL_RERANK_URL="http://100.x.y.z:8105"
export SCOS_MODEL_CODE_URL="http://100.x.y.z:8106"

export SCOS_MODEL_TEXT_MODEL="BAAI/bge-m3"
export SCOS_MODEL_TEXT_REVISION="main"
export SCOS_MODEL_TEXT_DIMENSIONS="1024"

export SCOS_MODEL_RERANK_MODEL="BAAI/bge-reranker-v2-m3"
export SCOS_MODEL_RERANK_REVISION="main"

export SCOS_MODEL_CODE_MODEL="BAAI/bge-code-v1"
export SCOS_MODEL_CODE_REVISION="main"
export SCOS_MODEL_CODE_DIMENSIONS="1536"

export SCOS_MODEL_TEXT_REQUIRED="true"
export SCOS_MODEL_RERANK_REQUIRED="false"
export SCOS_MODEL_CODE_REQUIRED="false"
```

Replace `100.x.y.z` with the Tailscale IPv4 address of the inference host.

Then start SCOS without any local model profile:

```bash
cd deploy/compose

docker compose \
  -f docker-compose.yml \
  -f docker-compose.models.yml \
  up -d model-runtime embeddings-worker retrieval-orchestrator
```

Because no `text-model`, `rerank-model`, or `code-model` profile is enabled, the heavyweight TEI containers are not started on the Pi.

## Direct backend checks

From another machine on the same Tailscale network:

```bash
curl -sS "http://100.x.y.z:8104/embed" \
  -H 'Content-Type: application/json' \
  -d '{"inputs":"SCOS memory retrieval"}'
```

The response should contain one dense vector.

For the reranker:

```bash
curl -sS "http://100.x.y.z:8105/rerank" \
  -H 'Content-Type: application/json' \
  -d '{"query":"SCOS memory","texts":["unrelated text","SCOS stores memory in PostgreSQL"],"raw_scores":false}'
```

The SCOS-related candidate should rank above the unrelated candidate.

## Migration behavior

Enabling the model overlay changes the active text space to:

```text
model = BAAI/bge-m3
revision = main
dimensions = 1024
```

The embeddings worker does not treat an older BGE-small vector as current. It queues that memory version for re-embedding, writes the BGE-M3 vector and removes stale model/revision/dimension rows for that version in the same database transaction.

`chunk_content_hash` is SHA-256 of the source content, so content identity is independent of the generated vector.

During a rolling re-embedding, old 384-dimensional rows may still exist for versions that have not reached the worker yet. Until retrieval is upgraded to explicitly select the active model space, a query encountering an old-dimensional row can cause pgvector semantic search to degrade. Exact, FTS, relation and session retrieval remain available. Do not declare a BGE-M3 migration complete until the embedding backlog is zero.

## Production pinning

The overlay defaults model revisions to `main` for easy first deployment. Production should pin each model to an exact Hugging Face commit revision and set the same revision in both the TEI launch command and SCOS environment. This prevents a mutable `main` branch from silently changing the vector space beneath an existing index.

TEI accepts `--revision <commit>` when serving a Hub model.

## What is live in this first integration

Working path:

```text
memory version
  -> embeddings-worker
  -> model-runtime /v1/embed/text
  -> BGE-M3 TEI
  -> validated 1024-d vector
  -> PostgreSQL memory_embeddings
  -> retrieval-orchestrator pgvector search
```

Available runtime endpoints, ready for the next integration phase:

```text
/v1/rerank     -> BGE reranker
/v1/embed/code -> BGE-Code
```

The retrieval orchestrator still needs its second-stage reranker call wired in, and code-intelligence still needs dense BGE-Code persistence/fusion. Those changes are tracked in `docs/superpowers/plans/2026-09-09-model-runtime-bge.md` rather than being falsely described here as already active.
