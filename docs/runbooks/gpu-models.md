# GPU-only retrieval models

On a host that runs models in GPU memory only, the embedding model (bge-m3) and the reranker (bge-reranker-v2-m3) are served by llama.cpp's Vulkan build (`deploy/compose/docker-compose.models-vulkan.yml`). The overlay is used instead of the CPU TEI profiles in `docker-compose.models.yml` (`text-model`, `rerank-model`, `code-model`), which must stay disabled on such a host. model-runtime talks to llama.cpp with `SCOS_MODEL_*_PROTOCOL=llamacpp`.

## Models

| File | Source | SHA-256 |
|---|---|---|
| `bge-m3-Q8_0.gguf` | `huggingface.co/gpustack/bge-m3-GGUF` | `950f4a8e5e19477a6d3c26d2f162233c20002c601f75e4b002e3239997821167` |
| `bge-reranker-v2-m3-Q8_0.gguf` | `huggingface.co/gpustack/bge-reranker-v2-m3-GGUF` | `a43c7c9b11a4c1517e5bf95151960e1621d1b72f7a493364b01e386cf1aaa1d3` |

Download both files into `SCOS_GGUF_DIR`, then check them with `sha256sum` against the table before starting the overlay.

## Configuration

- `SCOS_GGUF_DIR`: the directory holding the GGUF files.
- `SCOS_GPU_RENDER_GID`, `SCOS_GPU_VIDEO_GID`: the host's `render` and `video` group ids, from `getent group render video`. These give the containers access to `/dev/dri`.
- `SCOS_GPU_DEVICE` (default `Vulkan0`): the device that `llama-server --list-devices` reports.

## Placement

Verified on kubuntux, an AMD Radeon RX 470 with 4 GB of VRAM running RADV POLARIS10.

**Each model:**
- All 25 of 25 layers are offloaded.
- 598.6 MiB of weights are in `Vulkan0` VRAM. No model buffer is in host memory.
- The compute buffer is 80 MiB of VRAM.
- On the host there is a 24 MiB staging buffer and a 1 MiB output buffer.
- The server process uses about 245 MiB of RSS (HTTP, tokenisation and the Vulkan driver) on one thread.

**Both models together:** 1290 MiB of VRAM.

To check placement on any host:
1. Run `llama-server` with `-lv 4`.
2. Confirm `offloaded N/N layers to GPU`.
3. Confirm the only `model buffer size` line names the GPU device.

The 512 MB container memory cap is smaller than the model weights. A build that fell back to CPU inference therefore fails to load instead of running on the CPU.

## Changing models

Each model's revision is set by `SCOS_MODEL_TEXT_GGUF_REVISION` and `SCOS_MODEL_RERANK_GGUF_REVISION` (default `gguf-q8_0`). It names the quantised file, so its vectors form their own embedding space. Changing it makes the embeddings worker re-embed every memory with the new model.
