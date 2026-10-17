"""Tests for the consolidation worker module."""

import importlib
import sys
import unittest
from unittest.mock import MagicMock, patch
from pathlib import Path
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation.models import EvidenceReference, MemoryCandidate, MemoryClass, SensitivityClass
from scos_consolidation.promoter import PromotionDecision


class WorkerHelpersTests(unittest.TestCase):
    def test_database_url_requires_tls(self):
        with patch.dict("sys.modules", {"psycopg": MagicMock(), "psycopg.rows": MagicMock(), "psycopg.sql": MagicMock()}):
            import scos_consolidation.worker as w
            importlib.reload(w)
            w.require_tls_database_url("postgres://user:pass@db/scos?sslmode=require")
            for url in ("postgres://user:pass@db/scos", "postgres://user:pass@db/scos?sslmode=disable", "postgres://user:pass@db/scos?sslmode=prefer"):
                with self.assertRaises(SystemExit):
                    w.require_tls_database_url(url)
    def test_worker_module_structure(self):
        """Verify the consolidation worker exports all required cycle handlers."""
        with patch.dict("sys.modules", {"psycopg": MagicMock(), "psycopg.rows": MagicMock(), "psycopg.sql": MagicMock()}):
            import scos_consolidation.worker as w
            importlib.reload(w)
            self.assertTrue(hasattr(w, "main"))
            self.assertTrue(hasattr(w, "_process_cycle"))
            self.assertTrue(hasattr(w, "_claim_jobs"))
            self.assertTrue(hasattr(w, "_process_job"))
            self.assertTrue(hasattr(w, "_promote_candidate"))
            self.assertTrue(hasattr(w, "_write_candidate"))

    def test_db_promotion_persists_evidence_and_automatic_review(self):
        with patch.dict("sys.modules", {"psycopg": MagicMock(), "psycopg.rows": MagicMock(), "psycopg.sql": MagicMock()}):
            import scos_consolidation.worker as w
            importlib.reload(w)

            candidate = MemoryCandidate(
                candidate_id=uuid4(),
                tenant_id=uuid4(),
                namespace_id=uuid4(),
                source_event_id=uuid4(),
                memory_class=MemoryClass.SEMANTIC,
                title="Test knowledge",
                content="This is test content.",
                authority_level=70,
                confidence_score=0.85,
                sensitivity_class=SensitivityClass.INTERNAL,
                evidence=[EvidenceReference(kind="event", reference_id="event-1")],
                metadata={"actor_id": str(uuid4())},
            )
            promotion = PromotionDecision(candidate.candidate_id, "promote", "safe")
            conn = MagicMock()

            w._promote_candidate(conn, candidate, promotion)

            statements = [call.args[0] for call in conn.cursor.return_value.__enter__.return_value.execute.call_args_list]
            self.assertTrue(any("INSERT INTO memory_evidence" in statement for statement in statements))
            self.assertTrue(any("INSERT INTO memory_reviews" in statement for statement in statements))
            review_call = next(
                call for call in conn.cursor.return_value.__enter__.return_value.execute.call_args_list
                if "INSERT INTO memory_reviews" in call.args[0]
            )
            self.assertEqual(review_call.args[1][3], "approved")

    def test_a_job_without_a_turn_is_dead_lettered_with_its_reason(self):
        with patch.dict("sys.modules", {"psycopg": MagicMock(), "psycopg.rows": MagicMock(), "psycopg.sql": MagicMock()}):
            import scos_consolidation.worker as w
            importlib.reload(w)
            conn = MagicMock()
            job = {"id": uuid4(), "tenant_id": uuid4(), "job_type": "turn.completed", "payload": {"session_id": "s"}, "attempts": 1}
            w._process_job(conn, job)
            calls = conn.cursor.return_value.__enter__.return_value.execute.call_args_list
            update = next(c for c in calls if "UPDATE outbox_jobs" in c.args[0])
            self.assertEqual(update.args[1][0], "dead_letter")
            dead = next(c for c in calls if "INSERT INTO outbox_dead_letters" in c.args[0])
            self.assertEqual(dead.args[1][4], "job payload has no turn_id")

    def test_a_failure_is_retried_until_attempts_run_out(self):
        with patch.dict("sys.modules", {"psycopg": MagicMock(), "psycopg.rows": MagicMock(), "psycopg.sql": MagicMock()}):
            import scos_consolidation.worker as w
            importlib.reload(w)
            for attempts, status, dead_letters in ((2, "retryable", 0), (w.MAX_ATTEMPTS, "dead_letter", 1)):
                conn = MagicMock()
                w._mark_failed(conn, {"id": uuid4(), "tenant_id": uuid4(), "payload": {"turn_id": "t"}, "attempts": attempts}, "boom")
                calls = conn.cursor.return_value.__enter__.return_value.execute.call_args_list
                self.assertEqual(calls[0].args[1][0], status)
                self.assertEqual(sum("outbox_dead_letters" in c.args[0] for c in calls), dead_letters)


if __name__ == "__main__":
    unittest.main()
