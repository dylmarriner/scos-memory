"""Tests for the consolidation worker module."""

import importlib
import sys
import unittest
from unittest.mock import MagicMock, patch
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))


class WorkerHelpersTests(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()

