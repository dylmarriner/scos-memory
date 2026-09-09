"""Unit tests for the SCOS embeddings worker (no model / no DB required)."""

import os
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
from scos_embeddings import worker  # noqa: E402


class VectorLiteralTests(unittest.TestCase):
    def test_vector_literal_format(self):
        lit = worker._vector_literal([1.5, -0.25, 3.0])
        self.assertEqual(lit, "[1.50000000,-0.25000000,3.00000000]")

    def test_content_hash_is_hash_of_source_content(self):
        h1 = worker._content_hash_for("exact source text")
        h2 = worker._content_hash_for("exact source text")
        h3 = worker._content_hash_for("changed source text")
        self.assertEqual(h1, h2)
        self.assertNotEqual(h1, h3)


class TLSURLTests(unittest.TestCase):
    def test_accepts_sslmode_require(self):
        worker.require_tls_database_url(
            "postgres://scos:pw@postgres:5432/scos_memory?sslmode=require"
        )
        worker.require_tls_database_url(
            "postgresql://scos:pw@db:5432/x?sslmode=verify-full&application_name=scos"
        )

    def test_rejects_plaintext(self):
        with self.assertRaises(ValueError):
            worker.require_tls_database_url("postgres://scos:pw@localhost:5432/db")
        with self.assertRaises(ValueError):
            worker.require_tls_database_url(
                "postgres://scos:pw@h:5432/db?sslmode=disable"
            )


class RemainingBatchTests(unittest.TestCase):
    def test_unembedded_query_requires_current_model_space(self):
        class FakeConn:
            def cursor(self, row_factory=None):
                return self

            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

            def execute(self, sql, params):
                self._sql = sql
                self._params = params

            def fetchall(self):
                return [{"id": "1", "content": "x"}]

        conn = FakeConn()
        rows = worker._unembedded_batch(
            conn, 10, "BAAI/bge-m3", "rev-a", 1024
        )
        self.assertEqual(len(rows), 1)
        self.assertIn("me.model = %s", conn._sql)
        self.assertIn("me.model_revision = %s", conn._sql)
        self.assertIn("me.dimensions = %s", conn._sql)
        self.assertEqual(conn._params, ("BAAI/bge-m3", "rev-a", 1024, 10))


class EmbedTextsTests(unittest.TestCase):
    def test_returns_none_when_local_embedder_disabled(self):
        with patch.object(worker, "MODEL_RUNTIME_URL", ""):
            with patch.object(worker, "get_embedder", return_value=None):
                self.assertIsNone(worker.embed_texts(["hi"]))

    def test_returns_vectors_when_local_embedder_available(self):
        class FakeEmbedder:
            def embed(self, texts, batch_size=0):
                for _ in texts:
                    yield [float(i) for i in range(3)]

        with patch.object(worker, "MODEL_RUNTIME_URL", ""):
            with patch.object(worker, "MODEL_DIMENSIONS", 3):
                with patch.object(
                    worker, "get_embedder", return_value=FakeEmbedder()
                ):
                    vecs = worker.embed_texts(["a", "b"])
        self.assertEqual(len(vecs), 2)
        self.assertEqual(len(vecs[0]), 3)
        self.assertEqual(type(vecs[0][0]), float)

    def test_runtime_vectors_are_used_when_configured(self):
        with patch.object(worker, "MODEL_RUNTIME_URL", "https://model-runtime:8103"):
            with patch.object(
                worker, "_runtime_embed_text", return_value=[0.1, 0.2, 0.3]
            ) as runtime_embed:
                vecs = worker.embed_texts(["a", "b"])
        self.assertEqual(vecs, [[0.1, 0.2, 0.3], [0.1, 0.2, 0.3]])
        self.assertEqual(runtime_embed.call_count, 2)

    def test_local_dimension_mismatch_fails_closed(self):
        class FakeEmbedder:
            def embed(self, texts, batch_size=0):
                yield [0.1, 0.2]

        with patch.object(worker, "MODEL_RUNTIME_URL", ""):
            with patch.object(worker, "MODEL_DIMENSIONS", 3):
                with patch.object(
                    worker, "get_embedder", return_value=FakeEmbedder()
                ):
                    self.assertIsNone(worker.embed_texts(["question"]))


class ReplacementTests(unittest.TestCase):
    class FakeCursor:
        def __init__(self, rowcount=0):
            self.calls = []
            self.rowcount = rowcount

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def execute(self, sql, params):
            self.calls.append((sql, params))

    class FakeConn:
        def __init__(self, rowcount=0):
            self.cur = ReplacementTests.FakeCursor(rowcount=rowcount)

        def cursor(self, row_factory=None):
            return self.cur

    def test_purge_removes_every_non_active_space(self):
        conn = self.FakeConn(rowcount=4)
        removed = worker._purge_stale_spaces(
            conn, "BAAI/bge-m3", "rev-a", 1024
        )
        self.assertEqual(removed, 4)
        self.assertEqual(len(conn.cur.calls), 1)
        sql, params = conn.cur.calls[0]
        self.assertIn("DELETE FROM memory_embeddings", sql)
        self.assertIn("model <> %s", sql)
        self.assertIn("model_revision <> %s", sql)
        self.assertIn("dimensions <> %s", sql)
        self.assertEqual(params, ("BAAI/bge-m3", "rev-a", 1024))

    def test_replace_embedding_deletes_stale_spaces_before_insert(self):
        conn = self.FakeConn()
        worker._replace_embedding(
            conn,
            version_id="v1",
            content="source content",
            embedding=[0.1, 0.2, 0.3],
            model="BAAI/bge-m3",
            revision="rev-a",
        )
        self.assertEqual(len(conn.cur.calls), 2)
        self.assertIn("DELETE FROM memory_embeddings", conn.cur.calls[0][0])
        self.assertIn("INSERT INTO memory_embeddings", conn.cur.calls[1][0])


class RerankCandidatesTests(unittest.TestCase):
    def test_returns_none_when_disabled(self):
        with patch.object(worker, "get_reranker", return_value=None):
            self.assertIsNone(worker.rerank_candidates("q", ["a", "b"]))

    def test_returns_none_for_empty_candidates(self):
        class FakeReranker:
            def rerank(self, query, candidates):
                return []

        with patch.object(worker, "get_reranker", return_value=FakeReranker()):
            self.assertIsNone(worker.rerank_candidates("q", []))

    def test_returns_float_scores_when_available(self):
        class FakeReranker:
            def rerank(self, query, candidates):
                return [0.9, 0.1]

        with patch.object(worker, "get_reranker", return_value=FakeReranker()):
            scores = worker.rerank_candidates("q", ["a", "b"])
            self.assertEqual(scores, [0.9, 0.1])
            self.assertEqual(type(scores[0]), float)


if __name__ == "__main__":
    unittest.main()
