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


if __name__ == "__main__":
    unittest.main()
