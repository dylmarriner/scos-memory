"""SCOS vector-memory embedding worker.

Closes the vector write-path gap that left ``memory_embeddings`` permanently
empty: the retrieval-orchestrator runs real cosine-similarity queries against
``memory_embeddings`` but nothing ever populated the table. This worker:

1. Backfills every ``memory_versions.content`` row that lacks an embedding.
2. Runs continuously (bounded poll) to embed newly-created memory versions.
3. Serves ``POST /v1/embed`` returning a single query embedding, so callers
   can pass ``query_embedding`` into ``retrieval-orchestrator`` and get true
   semantic (vector) search instead of the silent full-text fallback.

Embedding model is local ONNX (fastembed, ``BAAI/bge-small-en-v1.5``, 384-d),
downloaded on first use and cached in the container's ``SCOS_EMBED_CACHE``.
No external API key is required — consistent with the fabric's "deterministic
/ local when no key" philosophy.

Usage:
    SCOS_DATABASE_URL="postgresql://...?sslmode=require" \\
        SCOS_EMBEDDINGS_HTTP_ADDR=":8102" \\
        python -m scos_embeddings.worker
"""

from __future__ import annotations

import json
import logging
import os
import threading
import time
from typing import Any
from urllib.parse import urlparse

import psycopg

try:
    from fastembed import TextEmbedding
except Exception:  # pragma: no cover - import guard for test environments
    TextEmbedding = None

logger = logging.getLogger("scos_embeddings")

# ── configuration ─────────────────────────────────────────────────────────

DATABASE_URL = os.environ.get(
    "SCOS_DATABASE_URL",
    os.environ.get("DATABASE_URL", ""),
)
POLL_SECONDS = float(os.environ.get("SCOS_EMBEDDINGS_POLL_SECONDS", "5"))
BATCH_SIZE = int(os.environ.get("SCOS_EMBEDDINGS_BATCH_SIZE", "64"))
HTTP_ADDR = os.environ.get("SCOS_EMBEDDINGS_HTTP_ADDR", ":8102")

# Local ONNX model (384-d). bge-small is fast on CPU and produces good
# cosine-similarity semantics for short memory facts + queries.
MODEL_NAME = os.environ.get("SCOS_EMBED_MODEL", "BAAI/bge-small-en-v1.5")
CACHE_DIR = os.environ.get("SCOS_EMBED_CACHE", "/var/lib/scos/embeddings")
MODEL_REV = os.environ.get("SCOS_EMBED_MODEL_REVISION", "local")

_embedder: Any = None
_embedder_lock = threading.Lock()


def require_tls_database_url(value: str) -> None:
    parsed = urlparse(value)
    sslmodes = [
        seg.split("=", 1)[1]
        for seg in parsed.query.split("&")
        if seg.lower().startswith("sslmode=")
    ]
    if (
        parsed.scheme not in {"postgres", "postgresql"}
        or not parsed.hostname
        or len(sslmodes) != 1
        or sslmodes[0].lower() not in {"require", "verify-ca", "verify-full"}
    ):
        raise ValueError(
            "SCOS_DATABASE_URL must be a postgres:// URL with sslmode=require or stronger"
        )


def get_embedder():
    """Lazily load the fastembed model (thread-safe). Returns None if the
    optional dependency/model is unavailable so the worker degrades gracefully
    instead of crashing the fleet."""
    global _embedder
    if _embedder is not None:
        return _embedder
    if TextEmbedding is None:
        logger.warning("fastembed not installed; embeddings disabled")
        return None
    with _embedder_lock:
        if _embedder is None:
            try:
                _embedder = TextEmbedding(
                    MODEL_NAME,
                    cache_dir=CACHE_DIR,
                )
                logger.info("embedding model loaded: %s (384-d)", MODEL_NAME)
            except Exception as exc:  # pragma: no cover
                logger.warning("failed to load embedding model (%s); vector write disabled", exc)
                _embedder = None
    return _embedder


def embed_texts(texts: list[str]) -> list[list[float]] | None:
    em = get_embedder()
    if em is None:
        return None
    out = []
    for vec in em.embed(texts, batch_size=BATCH_SIZE):
        out.append([float(x) for x in vec])
    return out


# ── database helpers ──────────────────────────────────────────────────────

def _unembedded_batch(conn: psycopg.Connection, limit: int) -> list[dict[str, Any]]:
    """memory_versions rows whose most recent embedding (any model) is absent,
    i.e. no row in memory_embeddings for that version id."""
    with conn.cursor(row_factory=psycopg.rows.dict_row) as cur:
        cur.execute(
            """
            SELECT mv.id::text AS id, mv.content
            FROM memory_versions mv
            WHERE NOT EXISTS (
                SELECT 1 FROM memory_embeddings me
                WHERE me.memory_version_id = mv.id
            )
            ORDER BY mv.created_at ASC
            LIMIT %s
            """,
            (limit,),
        )
        return cur.fetchall()


def _insert_embedding(
    conn: psycopg.Connection,
    version_id: str,
    embedding: list[float],
) -> None:
    with conn.cursor() as cur:
        cur.execute(
            """
            INSERT INTO memory_embeddings
                (memory_version_id, provider, model, model_revision, dimensions,
                 chunk_number, chunk_content_hash, embedding)
            VALUES (%s, %s, %s, %s, %s, 0, %s, %s::vector)
            ON CONFLICT (memory_version_id, model, model_revision, chunk_number)
            DO UPDATE SET embedding = EXCLUDED.embedding,
                          created_at = now()
            """,
            (
                version_id,
                "fastembed",
                MODEL_NAME,
                MODEL_REV,
                len(embedding),
                _content_hash_for(version_id, embedding),
                _vector_literal(embedding),
            ),
        )


def _content_hash_for(version_id: str, embedding: list[float]) -> str:
    import hashlib
    h = hashlib.sha256()
    h.update(version_id.encode())
    for x in embedding:
        h.update(str(x).encode())
    return h.hexdigest()


def _vector_literal(values: list[float]) -> str:
    # pgvector literal: [0.1,0.2,...]
    return "[" + ",".join(f"{v:.6f}" for v in values) + "]"


def run_cycle(conn: psycopg.Connection) -> int:
    """Embed one batch of unembedded memory versions. Returns rows written."""
    batch = _unembedded_batch(conn, BATCH_SIZE)
    if not batch:
        return 0
    embeddings = embed_texts([row["content"] for row in batch])
    if embeddings is None:
        return 0
    written = 0
    for row, vec in zip(batch, embeddings):
        if not vec:
            continue
        _insert_embedding(conn, row["id"], vec)
        written += 1
    conn.commit()
    return written


# ── main loop + HTTP serving ─────────────────────────────────────────────
def _serve_http() -> None:
    try:
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, format: str, *args) -> None:  # noqa: A002  # pragma: no cover
                return

            def _write(self, code: int, payload) -> None:
                body = json.dumps(payload).encode("utf-8")
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):  # noqa: N802
                if self.path != "/v1/embed":
                    self._write(404, {"error": "not found"})
                    return
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length) if length else b"{}"
                try:
                    req = json.loads(body.decode("utf-8") or "{}")
                except ValueError:
                    self._write(400, {"error": "invalid json"})
                    return
                text = req.get("input") or req.get("query") or ""
                if isinstance(text, list):
                    text = " ".join(str(t) for t in text)
                if not text.strip():
                    self._write(400, {"error": "empty input"})
                    return
                vec = embed_texts([text])
                if vec is None:
                    self._write(503, {"error": "embedder unavailable"})
                    return
                self._write(200, {
                    "embedding": vec[0],
                    "model": MODEL_NAME,
                    "dimensions": len(vec[0]),
                    "provider": "fastembed",
                })

            def do_GET(self):  # noqa: N802
                if self.path == "/health/live":
                    self._write(200, {"status": "alive"})
                elif self.path == "/health/ready":
                    ok = get_embedder() is not None
                    self._write(200 if ok else 503, {"ready": ok})
                else:
                    self._write(404, {"error": "not found"})

        host, _, port = HTTP_ADDR.partition(":")
        server = ThreadingHTTPServer((host or "0.0.0.0", int(port or 8102)), Handler)
        logger.info("query-embedding endpoint listening on %s", HTTP_ADDR)
        server.serve_forever()
    except Exception as exc:  # pragma: no cover
        logger.warning("HTTP server failed to start (%s); continuing worker only", exc)


def main() -> None:
    logging.basicConfig(
        level=getattr(logging, os.environ.get("SCOS_LOG_LEVEL", "info").upper()),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    require_tls_database_url(DATABASE_URL)

    # Warm the model so /health/ready reports correctly and the first batch is fast.
    _ = get_embedder()

    threading.Thread(target=_serve_http, daemon=True).start()

    with psycopg.connect(DATABASE_URL) as conn:
        while True:
            try:
                written = run_cycle(conn)
                if written:
                    logger.info("embedded %d memory version(s)", written)
            except Exception as exc:  # pragma: no cover
                conn.rollback()
                logger.warning("cycle failed: %s", exc)
            time.sleep(POLL_SECONDS)


if __name__ == "__main__":
    main()
