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
        self.assertEqual(lit, "[1.500000,-0.250000,3.000000]")

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
            worker.require_tls_database_url("postgres://scos:pw@h:5432/db?sslmode=disable")


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
        rows = worker._unembedded_batch(conn, 10, "BAAI/bge-m3", "rev-a", 1024)
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
            with patch.object(worker, "get_embedder", return_value=FakeEmbedder()):
                vecs = worker.embed_texts(["a", "b"])
        self.assertEqual(len(vecs), 2)
        self.assertEqual(len(vecs[0]), 3)
        self.assertEqual(type(vecs[0][0]), float)

    def test_query_embed_endpoint_payload_shape(self):
        class FakeEmbedder:
            def embed(self, texts, batch_size=0):
                for _ in texts:
                    yield [0.1, 0.2, 0.3]

        with patch.object(worker, "MODEL_RUNTIME_URL", ""):
            with patch.object(worker, "get_embedder", return_value=FakeEmbedder()):
                vec = worker.embed_texts(["question"])
        self.assertEqual(len(vec[0]), 3)


class ReplacementTests(unittest.TestCase):
    def test_replace_embedding_deletes_stale_spaces_before_insert(self):
        class FakeCursor:
            def __init__(self):
                self.calls = []

            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

            def execute(self, sql, params):
                self.calls.append((sql, params))

        class FakeConn:
            def __init__(self):
                self.cur = FakeCursor()

            def cursor(self):
                return self.cur

        conn = FakeConn()
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


if __name__ == "__main__":
    unittest.main()
