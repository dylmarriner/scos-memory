FROM python:3.12-slim AS builder
RUN pip install --no-cache-dir build hatchling
WORKDIR /build
COPY workers/consolidation/pyproject.toml .
COPY workers/consolidation/src/ src/
RUN python -m build --wheel --outdir /dist

FROM python:3.12-slim
RUN addgroup --system scos && adduser --system --ingroup scos scos && \
    pip install --no-cache-dir "psycopg[binary]>=3.2,<4" "pydantic>=2.10,<3"
COPY --from=builder /dist/*.whl /tmp/
RUN pip install --no-cache-dir /tmp/*.whl && rm /tmp/*.whl
USER scos
ENV SCOS_LOG_LEVEL=info
ENTRYPOINT ["python", "-m", "scos_consolidation.worker"]
