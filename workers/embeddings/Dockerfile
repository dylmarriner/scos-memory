FROM python:3.12-slim AS builder
RUN pip install --no-cache-dir build hatchling
WORKDIR /build
COPY workers/embeddings/pyproject.toml .
COPY workers/embeddings/src/ src/
RUN python -m build --wheel --outdir /dist

FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* && \
    addgroup --system scos && adduser --system --ingroup scos scos && \
    pip install --no-cache-dir \
      "psycopg[binary]>=3.2,<4" \
      "onnxruntime>=1.17,<2" \
      "fastembed>=0.8,<1"
COPY --from=builder /dist/*.whl /tmp/
RUN pip install --no-cache-dir /tmp/*.whl && rm /tmp/*.whl && \
    mkdir -p /var/lib/scos/embeddings && chown -R scos:scos /var/lib/scos
USER scos
ENV SCOS_LOG_LEVEL=info \
    SCOS_EMBED_CACHE=/var/lib/scos/embeddings \
    HOME=/var/lib/scos \
    HF_HOME=/var/lib/scos/.cache/huggingface \
    XET_CACHE=/var/lib/scos/.cache/xet
ENTRYPOINT ["python", "-m", "scos_embeddings.worker"]
