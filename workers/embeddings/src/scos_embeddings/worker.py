"""SCOS vector-memory embedding worker.

The worker owns persistence of the active text embedding space. Query and
backfill inference can be supplied by the internal SCOS model-runtime service
or, when that URL is unset, by a local FastEmbed model.

The active vector space is explicit: model + revision + dimensions. Existing
vectors from another space do not count as current and are replaced atomically
when a memory version is re-embedded. The existing local cross-encoder rerank
endpoint is retained as a compatibility/degraded-mode path.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import ssl
import threading
import time
from typing import Any
from urllib import error as urlerror
from urllib import request as urlrequest
from urllib.parse import urlparse

import psycopg

try:
    from fastembed import TextEmbedding
except Exception:  # pragma: no cover - import guard for test environments
    TextEmbedding = None

try:
    from fastembed.rerank.cross_encoder import TextCrossEncoder
except Exception:  # pragma: no cover - import guard for test environments
    TextCrossEncoder = None

logger = logging.getLogger("scos_embeddings")

DATABASE_URL = os.environ.get(
    "SCOS_DATABASE_URL",
    os.environ.get("DATABASE_URL", ""),
)
POLL_SECONDS = float(os.environ.get("SCOS_EMBEDDINGS_POLL_SECONDS", "5"))
BATCH_SIZE = int(os.environ.get("SCOS_EMBEDDINGS_BATCH_SIZE", "64"))
HTTP_ADDR = os.environ.get("SCOS_EMBEDDINGS_HTTP_ADDR", ":8102")

# Keep the base-compose default compatible with the current repository. The
# hosted model overlay explicitly switches these values to BGE-M3.
MODEL_NAME = os.environ.get("SCOS_EMBED_MODEL", "BAAI/bge-small-en-v1.5")
_legacy_bge_small = MODEL_NAME == "BAAI/bge-small-en-v1.5"
MODEL_REV = os.environ.get(
    "SCOS_EMBED_MODEL_REVISION", "local" if _legacy_bge_small else "main"
)
MODEL_DIMENSIONS = int(
    os.environ.get("SCOS_EMBED_DIMENSIONS", "384" if _legacy_bge_small else "1024")
)
CACHE_DIR = os.environ.get("SCOS_EMBED_CACHE", "/var/lib/scos/embeddings")

MODEL_RUNTIME_URL = os.environ.get("SCOS_MODEL_RUNTIME_URL", "").rstrip("/")
MODEL_RUNTIME_TOKEN = os.environ.get("SCOS_INTERNAL_SERVICE_TOKEN", "")
MODEL_RUNTIME_CONTROL_TOKEN = os.environ.get("SCOS_CONTROL_PLANE_TOKEN", "")
MODEL_RUNTIME_CA_FILE = os.environ.get("SCOS_MODEL_RUNTIME_CA_FILE", "")
MODEL_RUNTIME_TIMEOUT_SECONDS = float(
    os.environ.get("SCOS_MODEL_RUNTIME_TIMEOUT_SECONDS", "60")
)

# Existing local cross-encoder compatibility path. The retrieval orchestrator
# uses the dedicated BGE reranker through model-runtime when configured, but
# this endpoint remains available so current callers are not regressed.
RERANK_MODEL_NAME = os.environ.get(
    "SCOS_RERANK_MODEL", "Xenova/ms-marco-MiniLM-L-6-v2"
)
RERANK_CACHE_DIR = os.environ.get("SCOS_RERANK_CACHE", "/var/lib/scos/embeddings")

_embedder: Any = None
_embedder_lock = threading.Lock()
_reranker: Any = None
_reranker_lock = threading.Lock()


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
    """Lazily load the configured local FastEmbed model."""
    global _embedder
    if _embedder is not None:
        return _embedder
    if TextEmbedding is None:
        logger.warning("fastembed not installed; local embeddings disabled")
        return None
    with _embedder_lock:
        if _embedder is None:
            try:
                _embedder = TextEmbedding(MODEL_NAME, cache_dir=CACHE_DIR)
                logger.info(
                    "local embedding model loaded: %s (%d-d expected)",
                    MODEL_NAME,
                    MODEL_DIMENSIONS,
                )
            except Exception as exc:  # pragma: no cover
                logger.warning("failed to load embedding model (%s)", exc)
                _embedder = None
    return _embedder


def get_reranker():
    """Lazily load the existing local FastEmbed cross-encoder."""
    global _reranker
    if _reranker is not None:
        return _reranker
    if TextCrossEncoder is None:
        logger.warning("fastembed cross-encoder not installed; rerank disabled")
        return None
    with _reranker_lock:
        if _reranker is None:
            try:
                _reranker = TextCrossEncoder(
                    RERANK_MODEL_NAME, cache_dir=RERANK_CACHE_DIR
                )
                logger.info("rerank model loaded: %s", RERANK_MODEL_NAME)
            except Exception as exc:  # pragma: no cover
                logger.warning("failed to load rerank model (%s); rerank disabled", exc)
                _reranker = None
    return _reranker


def rerank_candidates(query: str, candidates: list[str]) -> list[float] | None:
    """Score candidates through the retained local compatibility reranker."""
    ce = get_reranker()
    if ce is None or not candidates:
        return None
    return [float(score) for score in ce.rerank(query, candidates)]


def _runtime_ssl_context() -> ssl.SSLContext | None:
    if not MODEL_RUNTIME_URL.lower().startswith("https://"):
        return None
    if MODEL_RUNTIME_CA_FILE:
        return ssl.create_default_context(cafile=MODEL_RUNTIME_CA_FILE)
    return ssl.create_default_context()


def _runtime_embed_text(text: str) -> list[float]:
    payload = json.dumps({"input": text}).encode("utf-8")
    req = urlrequest.Request(
        MODEL_RUNTIME_URL + "/v1/embed/text",
        data=payload,
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    if MODEL_RUNTIME_TOKEN:
        req.add_header("Authorization", "Bearer " + MODEL_RUNTIME_TOKEN)
    if MODEL_RUNTIME_CONTROL_TOKEN:
        req.add_header(
            "X-SCOS-Control-Plane-Token", MODEL_RUNTIME_CONTROL_TOKEN
        )
    try:
        with urlrequest.urlopen(
            req,
            timeout=MODEL_RUNTIME_TIMEOUT_SECONDS,
            context=_runtime_ssl_context(),
        ) as resp:
            body = json.loads(resp.read().decode("utf-8"))
    except (urlerror.URLError, TimeoutError, ValueError) as exc:
        raise RuntimeError(f"model runtime unavailable: {exc}") from exc

    embedding = body.get("embedding")
    if not isinstance(embedding, list) or not embedding:
        raise RuntimeError("model runtime returned no embedding")
    if body.get("model") != MODEL_NAME:
        raise RuntimeError(
            f"model runtime returned {body.get('model')!r}, expected {MODEL_NAME!r}"
        )
    if body.get("model_revision") != MODEL_REV:
        raise RuntimeError(
            "model runtime revision does not match active embedding revision"
        )
    if int(body.get("dimensions") or 0) != MODEL_DIMENSIONS:
        raise RuntimeError(
            "model runtime dimensions do not match active embedding space"
        )
    return [float(value) for value in embedding]


def embed_texts(texts: list[str]) -> list[list[float]] | None:
    if MODEL_RUNTIME_URL:
        try:
            return [_runtime_embed_text(text) for text in texts]
        except Exception as exc:
            logger.warning("model-runtime embedding failed (%s)", exc)
            return None

    em = get_embedder()
    if em is None:
        return None
    out = []
    for vec in em.embed(texts, batch_size=BATCH_SIZE):
        values = [float(x) for x in vec]
        if len(values) != MODEL_DIMENSIONS:
            logger.warning(
                "local model returned %d dimensions, expected %d",
                len(values),
                MODEL_DIMENSIONS,
            )
            return None
        out.append(values)
    return out


def _unembedded_batch(
    conn: psycopg.Connection,
    limit: int,
    model: str,
    revision: str,
    dimensions: int,
) -> list[dict[str, Any]]:
    """Return memory versions missing the active model space."""
    with conn.cursor(row_factory=psycopg.rows.dict_row) as cur:
        cur.execute(
            """
            SELECT mv.id::text AS id, mv.content
            FROM memory_versions mv
            WHERE NOT EXISTS (
                SELECT 1 FROM memory_embeddings me
                WHERE me.memory_version_id = mv.id
                  AND me.model = %s
                  AND me.model_revision = %s
                  AND me.dimensions = %s
                  AND me.chunk_number = 0
                  AND me.embedding IS NOT NULL
            )
            ORDER BY mv.created_at ASC
            LIMIT %s
            """,
            (model, revision, dimensions, limit),
        )
        return cur.fetchall()


def _content_hash_for(content: str) -> str:
    return hashlib.sha256(content.encode("utf-8")).hexdigest()


def _vector_literal(values: list[float]) -> str:
    return "[" + ",".join(f"{value:.8f}" for value in values) + "]"


def _replace_embedding(
    conn: psycopg.Connection,
    version_id: str,
    content: str,
    embedding: list[float],
    model: str,
    revision: str,
) -> None:
    """Replace stale vector spaces for one memory version atomically."""
    dimensions = len(embedding)
    with conn.cursor() as cur:
        cur.execute(
            """
            DELETE FROM memory_embeddings
             WHERE memory_version_id = %s
               AND (model <> %s OR model_revision <> %s OR dimensions <> %s)
            """,
            (version_id, model, revision, dimensions),
        )
        cur.execute(
            """
            INSERT INTO memory_embeddings
                (memory_version_id, provider, model, model_revision, dimensions,
                 chunk_number, chunk_content_hash, embedding)
            VALUES (%s, %s, %s, %s, %s, 0, %s, %s::vector)
            ON CONFLICT (memory_version_id, model, model_revision, chunk_number)
            DO UPDATE SET embedding = EXCLUDED.embedding,
                          dimensions = EXCLUDED.dimensions,
                          chunk_content_hash = EXCLUDED.chunk_content_hash,
                          created_at = now()
            """,
            (
                version_id,
                "scos-model-runtime" if MODEL_RUNTIME_URL else "fastembed",
                model,
                revision,
                dimensions,
                _content_hash_for(content),
                _vector_literal(embedding),
            ),
        )


def run_cycle(conn: psycopg.Connection) -> int:
    """Embed one batch of versions missing the active text model space."""
    batch = _unembedded_batch(
        conn,
        BATCH_SIZE,
        MODEL_NAME,
        MODEL_REV,
        MODEL_DIMENSIONS,
    )
    if not batch:
        return 0
    embeddings = embed_texts([row["content"] for row in batch])
    if embeddings is None:
        return 0
    written = 0
    for row, vec in zip(batch, embeddings):
        if len(vec) != MODEL_DIMENSIONS:
            continue
        _replace_embedding(
            conn,
            version_id=row["id"],
            content=row["content"],
            embedding=vec,
            model=MODEL_NAME,
            revision=MODEL_REV,
        )
        written += 1
    conn.commit()
    return written


def _serve_http() -> None:
    try:
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, format: str, *args) -> None:  # noqa: A002
                return

            def _write(self, code: int, payload) -> None:
                body = json.dumps(payload).encode("utf-8")
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def _read_json(self) -> dict[str, Any] | None:
                length = int(self.headers.get("Content-Length") or 0)
                if length > 1 << 20:
                    self._write(413, {"error": "request too large"})
                    return None
                body = self.rfile.read(length) if length else b"{}"
                try:
                    value = json.loads(body.decode("utf-8") or "{}")
                except ValueError:
                    self._write(400, {"error": "invalid json"})
                    return None
                if not isinstance(value, dict):
                    self._write(400, {"error": "request must be an object"})
                    return None
                return value

            def do_POST(self):  # noqa: N802
                if self.path == "/v1/embed":
                    self._handle_embed()
                elif self.path == "/v1/rerank":
                    self._handle_rerank()
                else:
                    self._write(404, {"error": "not found"})

            def _handle_embed(self) -> None:
                req = self._read_json()
                if req is None:
                    return
                text = req.get("input") or req.get("query") or ""
                if isinstance(text, list):
                    text = " ".join(str(item) for item in text)
                if not str(text).strip():
                    self._write(400, {"error": "empty input"})
                    return
                vec = embed_texts([str(text)])
                if vec is None:
                    self._write(503, {"error": "embedder unavailable"})
                    return
                self._write(
                    200,
                    {
                        "embedding": vec[0],
                        "model": MODEL_NAME,
                        "model_revision": MODEL_REV,
                        "dimensions": len(vec[0]),
                        "provider": (
                            "scos-model-runtime"
                            if MODEL_RUNTIME_URL
                            else "fastembed"
                        ),
                    },
                )

            def _handle_rerank(self) -> None:
                req = self._read_json()
                if req is None:
                    return
                query = req.get("query") or ""
                candidates = req.get("candidates")
                if not isinstance(query, str) or not query.strip():
                    self._write(400, {"error": "query is required"})
                    return
                if (
                    not isinstance(candidates, list)
                    or not candidates
                    or not all(isinstance(candidate, str) for candidate in candidates)
                ):
                    self._write(
                        400,
                        {"error": "candidates must be a non-empty array of strings"},
                    )
                    return
                scores = rerank_candidates(query, candidates)
                if scores is None:
                    self._write(503, {"error": "reranker unavailable"})
                    return
                self._write(
                    200,
                    {"scores": scores, "model": RERANK_MODEL_NAME},
                )

            def do_GET(self):  # noqa: N802
                if self.path == "/health/live":
                    self._write(200, {"status": "alive"})
                elif self.path == "/health/ready":
                    if MODEL_RUNTIME_URL:
                        try:
                            vec = _runtime_embed_text("SCOS readiness probe")
                            ok = len(vec) == MODEL_DIMENSIONS
                        except Exception:
                            ok = False
                    else:
                        ok = get_embedder() is not None
                    self._write(200 if ok else 503, {"ready": ok})
                else:
                    self._write(404, {"error": "not found"})

        host, _, port = HTTP_ADDR.partition(":")
        server = ThreadingHTTPServer(
            (host or "0.0.0.0", int(port or 8102)), Handler
        )
        logger.info("embedding endpoint listening on %s", HTTP_ADDR)
        server.serve_forever()
    except Exception as exc:  # pragma: no cover
        logger.warning("HTTP server failed to start (%s); continuing worker only", exc)


def main() -> None:
    logging.basicConfig(
        level=getattr(logging, os.environ.get("SCOS_LOG_LEVEL", "info").upper()),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    require_tls_database_url(DATABASE_URL)
    if MODEL_DIMENSIONS <= 0:
        raise SystemExit("SCOS_EMBED_DIMENSIONS must be positive")

    if MODEL_RUNTIME_URL:
        logger.info(
            "using model runtime %s for %s revision=%s dimensions=%d",
            MODEL_RUNTIME_URL,
            MODEL_NAME,
            MODEL_REV,
            MODEL_DIMENSIONS,
        )
    else:
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
