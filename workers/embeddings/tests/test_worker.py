"""Unit tests for the SCOS embeddings worker (no model / no DB required)."""

import json
import unittest
from unittest.mock import patch

# Make the package importable without the optional deps installed.
import sys, os
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
from scos_embeddings import worker  # noqa: E402


class VectorLiteralTests(unittest.TestCase):
    def test_vector_literal_format(self):
        lit = worker._vector_literal([1.5, -0.25, 3.0])
        self.assertEqual(lit, "[1.500000,-0.250000,3.000000]")

    def test_content_hash_stable(self):
        h1 = worker._content_hash_for("abc", [1.0, 2.0])
        h2 = worker._content_hash_for("abc", [1.0, 2.0])
        h3 = worker._content_hash_for("abc", [1.0, 3.0])
        self.assertEqual(h1, h2)
        self.assertNotEqual(h1, h3)


class TLSURLTests(unittest.TestCase):
    def test_accepts_sslmode_require(self):
        # Regression: the old parser indexed q[1] on the whole "sslmode=require"
        # segment, yielding "s" and rejecting valid URLs.
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
            worker.require_tls_database_url("postgres://scos:pw@h:5432/db?sslmode=disable")


class RemainingBatchTests(unittest.TestCase):
    def test_unembedded_query_excludes_embedded(self):
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
        rows = worker._unembedded_batch(conn, 10)
        self.assertEqual(len(rows), 1)
        self.assertIn("NOT EXISTS", conn._sql)
        self.assertIn("memory_embeddings", conn._sql)


class EmbedTextsTests(unittest.TestCase):
    def test_returns_none_when_disabled(self):
        with patch.object(worker, "get_embedder", return_value=None):
            self.assertIsNone(worker.embed_texts(["hi"]))

    def test_returns_vectors_when_available(self):
        class FakeEmbedder:
            def embed(self, texts, batch_size=0):
                for _ in texts:
                    yield [float(i) for i in range(3)]

        with patch.object(worker, "get_embedder", return_value=FakeEmbedder()):
            vecs = worker.embed_texts(["a", "b"])
            self.assertEqual(len(vecs), 2)
            self.assertEqual(len(vecs[0]), 3)
            # float-typed
            self.assertEqual(type(vecs[0][0]), float)

    def test_query_embed_endpoint_returns_payload(self):
        class FakeEmbedder:
            def embed(self, texts, batch_size=0):
                for _ in texts:
                    yield [0.1, 0.2, 0.3]

        with patch.object(worker, "get_embedder", return_value=FakeEmbedder()):
            with patch.object(worker, "TextEmbedding"):
                vec = worker.embed_texts(["question"])
        self.assertEqual(len(vec[0]), 3)


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
