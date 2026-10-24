"""Unit tests for the SCOS embeddings worker (no model / no DB required)."""

import os
import sys
import unittest
from pathlib import Path
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


class EmbeddingSpaceTests(unittest.TestCase):
    class FakeCursor:
        def __init__(self):
            self.calls = []

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def execute(self, sql, params):
            self.calls.append((sql, params))

        def fetchone(self):
            return ("space-1",)

    class FakeConn:
        def __init__(self):
            self.cur = EmbeddingSpaceTests.FakeCursor()

        def cursor(self, row_factory=None):
            return self.cur

    def test_ensure_embedding_space_uses_full_identity(self):
        conn = self.FakeConn()
        space_id = worker._ensure_embedding_space(
            conn,
            provider="scos-model-runtime",
            model="BAAI/bge-m3",
            revision="rev-a",
            dimensions=1024,
            generation=2,
        )
        self.assertEqual(space_id, "space-1")
        sql, params = conn.cur.calls[0]
        self.assertIn("INSERT INTO embedding_spaces", sql)
        self.assertIn("generation", sql)
        self.assertIn("RETURNING id::text", sql)
        self.assertEqual(
            params,
            ("text", "scos-model-runtime", "BAAI/bge-m3", "rev-a", 1024, 2),
        )


class RemainingBatchTests(unittest.TestCase):
    def test_unembedded_query_requires_exact_space_id(self):
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
        rows = worker._unembedded_batch(conn, 10, "space-1")
        self.assertEqual(len(rows), 1)
        self.assertIn("scos_worker_unembedded_versions(%s::uuid, %s)", conn._sql)
        self.assertEqual(conn._params, ("space-1", 10))
        # The scan itself lives in the migration: it must match the exact
        # embedding space, never just the model name.
        migration = (Path(__file__).resolve().parents[3] / "database/migrations/000043_worker_functions.sql").read_text()
        scan = migration[migration.index("FUNCTION scos_worker_unembedded_versions"):migration.index("FUNCTION scos_worker_store_embedding")]
        self.assertIn("me.embedding_space_id = p_space_id", scan)
        self.assertNotIn("me.model", scan)


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
            self.cur = ReplacementTests.FakeCursor()

        def cursor(self, row_factory=None):
            return self.cur

    def test_replace_embedding_keeps_other_spaces_and_writes_space_id(self):
        conn = self.FakeConn()
        worker._replace_embedding(
            conn,
            version_id="v1",
            content="source content",
            embedding=[0.1, 0.2, 0.3],
            space_id="space-1",
            provider="scos-model-runtime",
            model="BAAI/bge-m3",
            revision="rev-a",
        )
        self.assertEqual(len(conn.cur.calls), 1)
        sql, params = conn.cur.calls[0]
        self.assertIn("scos_worker_store_embedding", sql)
        self.assertEqual(params[:2], ("v1", "space-1"))
        self.assertEqual(params[5], 3)
        migration = (Path(__file__).resolve().parents[3] / "database/migrations/000043_worker_functions.sql").read_text()
        store = migration[migration.index("FUNCTION scos_worker_store_embedding"):migration.index("FUNCTION scos_worker_expire_memories")]
        self.assertNotIn("DELETE FROM memory_embeddings", store)
        self.assertIn("ON CONFLICT (memory_version_id, embedding_space_id, chunk_number)", store)


class RunCycleTests(unittest.TestCase):
    class FakeConn:
        def __init__(self):
            self.commits = 0
            self.rollbacks = 0

        def commit(self):
            self.commits += 1

        def rollback(self):
            self.rollbacks += 1

    def test_cycle_resolves_space_once(self):
        conn = self.FakeConn()
        with patch.object(worker, "_ensure_embedding_space", return_value="space-1") as ensure:
            with patch.object(worker, "_activate_embedding_space", return_value=False) as activate:
                with patch.object(worker, "_unembedded_batch", return_value=[]) as batch:
                    written = worker.run_cycle(conn)
        self.assertEqual(written, 0)
        ensure.assert_called_once()
        activate.assert_called_once_with(conn, "space-1")
        batch.assert_called_once_with(conn, worker.BATCH_SIZE, "space-1")
        # The activation is committed on its own, then the empty batch.
        self.assertEqual(conn.commits, 2)

    def test_activation_survives_an_embedding_failure(self):
        conn = self.FakeConn()
        order = []
        conn.commit = lambda: order.append("commit")
        conn.rollback = lambda: order.append("rollback")
        with patch.object(worker, "_ensure_embedding_space", return_value="space-1"):
            with patch.object(worker, "_activate_embedding_space", side_effect=lambda *_: order.append("activate")):
                with patch.object(worker, "_unembedded_batch", return_value=[{"id": "v1", "content": "x"}]):
                    with patch.object(worker, "embed_texts_prefix", return_value=([], "backend down")):
                        with self.assertRaises(worker.EmbeddingUnavailable):
                            worker.run_cycle(conn)
        self.assertEqual(order, ["activate", "commit", "rollback"])

    def test_a_vector_of_the_wrong_dimension_fails_the_cycle(self):
        conn = self.FakeConn()
        stored = []
        with patch.object(worker, "_ensure_embedding_space", return_value="space-1"):
            with patch.object(worker, "_activate_embedding_space"):
                with patch.object(worker, "_unembedded_batch", return_value=[{"id": "v1", "content": "x"}, {"id": "v2", "content": "y"}]):
                    with patch.object(worker, "embed_texts_prefix", return_value=([[0.0] * worker.MODEL_DIMENSIONS, [0.0] * 3], None)):
                        with patch.object(worker, "_replace_embedding", side_effect=lambda *a, **k: stored.append(k)):
                            with self.assertRaises(worker.EmbeddingUnavailable):
                                worker.run_cycle(conn)
        self.assertEqual(stored, [])
        self.assertEqual(conn.rollbacks, 1)

    def test_versions_before_a_failing_one_are_kept_and_the_failure_is_reported(self):
        conn = self.FakeConn()
        stored = []
        batch = [{"id": "v1", "content": "x"}, {"id": "v2", "content": "too long"}, {"id": "v3", "content": "z"}]
        with patch.object(worker, "_ensure_embedding_space", return_value="space-1"):
            with patch.object(worker, "_activate_embedding_space"):
                with patch.object(worker, "_unembedded_batch", return_value=batch):
                    with patch.object(worker, "embed_texts_prefix", return_value=([[0.0] * worker.MODEL_DIMENSIONS], "HTTP Error 502")):
                        with patch.object(worker, "_replace_embedding", side_effect=lambda *a, **k: stored.append(k["version_id"])):
                            with self.assertRaises(worker.EmbeddingUnavailable) as raised:
                                worker.run_cycle(conn)
        self.assertEqual(stored, ["v1"])
        self.assertEqual(conn.commits, 2)
        self.assertIn("v2", str(raised.exception))

    def test_prefix_stops_at_the_first_failing_text(self):
        calls = []

        def fake(text):
            calls.append(text)
            if text == "bad":
                raise RuntimeError("model runtime unavailable: HTTP Error 502")
            return [0.0] * worker.MODEL_DIMENSIONS

        with patch.object(worker, "MODEL_RUNTIME_URL", "https://model-runtime:8103"):
            with patch.object(worker, "_runtime_embed_text", side_effect=fake):
                vectors, failure = worker.embed_texts_prefix(["a", "bad", "c"])
                self.assertIsNone(worker.embed_texts(["a", "bad"]))
        self.assertEqual(len(vectors), 1)
        self.assertIn("502", failure)
        self.assertEqual(calls, ["a", "bad", "a", "bad"])


class ActivationTests(unittest.TestCase):
    class FakeCursor:
        def __init__(self, rowcount):
            self.calls = []
            self.rowcount = rowcount

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def execute(self, sql, params):
            self.calls.append((sql, params))

    class FakeConn:
        def __init__(self, rowcount):
            self.cur = ActivationTests.FakeCursor(rowcount)

        def cursor(self, row_factory=None):
            return self.cur

    def test_activation_marks_the_space_unless_it_is_already_active(self):
        conn = self.FakeConn(rowcount=1)
        self.assertTrue(worker._activate_embedding_space(conn, "space-2"))
        sql, params = conn.cur.calls[0]
        self.assertIn("UPDATE embedding_spaces", sql)
        self.assertIn("SET activated_at", sql)
        self.assertIn("IS DISTINCT FROM", sql)
        self.assertEqual(params, ("space-2",))
        self.assertFalse(worker._activate_embedding_space(self.FakeConn(rowcount=0), "space-2"))

    def test_backlog_counts_against_the_active_space(self):
        migration = (Path(__file__).resolve().parents[3] / "database/migrations/000053_hierarchy_membership_and_active_embedding_space.sql").read_text()
        backlog = migration[migration.index("FUNCTION scos_worker_embedding_backlog()"):]
        self.assertIn("activated_at IS NOT NULL", backlog)
        self.assertIn("me.embedding_space_id = (SELECT id FROM active_space)", backlog)
        self.assertIn("me.chunk_number = 0", backlog)


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
