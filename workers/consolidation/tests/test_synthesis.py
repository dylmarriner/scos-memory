"""Tests for Dream-style Synthesis (background pattern distillation)."""

import os
import sys
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

# synthesis.py's except clause names psycopg.Error; a bare MagicMock isn't a
# valid exception class, so give the mocked module a real one.
_mock_psycopg = MagicMock()
_mock_psycopg.Error = type("Error", (Exception,), {})
with patch.dict("sys.modules", {"psycopg": _mock_psycopg, "psycopg.rows": MagicMock()}):
    from scos_consolidation import synthesis
    from scos_consolidation.synthesis import _deepseek_config, run_synthesis_cycle


class DeepseekConfigTests(unittest.TestCase):
    def test_disabled_without_api_key(self):
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("DEEPSEEK_API_KEY", None)
            self.assertIsNone(_deepseek_config())

    def test_enabled_with_defaults(self):
        with patch.dict(os.environ, {"DEEPSEEK_API_KEY": "sk-test"}, clear=False):
            os.environ.pop("DEEPSEEK_BASE_URL", None)
            os.environ.pop("DEEPSEEK_MODEL", None)
            api_key, base_url, model = _deepseek_config()
            self.assertEqual(api_key, "sk-test")
            self.assertEqual(base_url, "https://api.deepseek.com/v1")
            self.assertEqual(model, "deepseek-chat")


class RunSynthesisCycleTests(unittest.TestCase):
    def test_noop_without_api_key(self):
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("DEEPSEEK_API_KEY", None)
            with patch.object(synthesis, "_eligible_namespaces") as eligible:
                written = run_synthesis_cycle(MagicMock())
        self.assertEqual(written, 0)
        eligible.assert_not_called()

    def test_writes_pattern_from_valid_response(self):
        namespace_id = uuid4()
        fact_a, fact_b = str(uuid4()), str(uuid4())
        ns_row = {"namespace_id": namespace_id, "tenant_id": uuid4(), "acting_principal_id": uuid4(), "fact_count": 25}

        with patch.dict(os.environ, {"DEEPSEEK_API_KEY": "sk-test"}, clear=False), \
             patch.object(synthesis, "_eligible_namespaces", return_value=[ns_row]), \
             patch.object(synthesis, "_due_for_synthesis", return_value=True), \
             patch.object(synthesis, "_load_existing_patterns", return_value=[]), \
             patch.object(synthesis, "_load_facts_for_synthesis",
                   return_value=[{"id": fact_a, "content": "a"}, {"id": fact_b, "content": "b"}]), \
             patch.object(synthesis, "_call_deepseek", return_value={
                 "patterns": [{"content": "distilled pattern", "source_fact_ids": [fact_a, fact_b], "confidence": 0.8}]
             }), \
             patch.object(synthesis, "_propose_pattern") as insert_pattern, \
             patch.object(synthesis, "_mark_synthesis_run") as mark_run:
            written = run_synthesis_cycle(MagicMock())

        self.assertEqual(written, 1)
        insert_pattern.assert_called_once()
        mark_run.assert_called_once_with(unittest.mock.ANY, namespace_id, 1)

    def test_skips_pattern_with_fewer_than_two_known_sources(self):
        namespace_id = uuid4()
        fact_a = str(uuid4())
        ns_row = {"namespace_id": namespace_id, "tenant_id": uuid4(), "acting_principal_id": uuid4(), "fact_count": 25}

        with patch.dict(os.environ, {"DEEPSEEK_API_KEY": "sk-test"}, clear=False), \
             patch.object(synthesis, "_eligible_namespaces", return_value=[ns_row]), \
             patch.object(synthesis, "_due_for_synthesis", return_value=True), \
             patch.object(synthesis, "_load_existing_patterns", return_value=[]), \
             patch.object(synthesis, "_load_facts_for_synthesis",
                   return_value=[{"id": fact_a, "content": "a"}]), \
             patch.object(synthesis, "_call_deepseek", return_value={
                 "patterns": [{"content": "pattern", "source_fact_ids": [fact_a, "unknown-id"], "confidence": 0.8}]
             }), \
             patch.object(synthesis, "_propose_pattern") as insert_pattern, \
             patch.object(synthesis, "_mark_synthesis_run") as mark_run:
            written = run_synthesis_cycle(MagicMock())

        self.assertEqual(written, 0)
        insert_pattern.assert_not_called()
        mark_run.assert_called_once_with(unittest.mock.ANY, namespace_id, 0)

    def test_skips_namespace_not_due(self):
        namespace_id = uuid4()
        ns_row = {"namespace_id": namespace_id, "tenant_id": uuid4(), "acting_principal_id": uuid4(), "fact_count": 25}

        with patch.dict(os.environ, {"DEEPSEEK_API_KEY": "sk-test"}, clear=False), \
             patch.object(synthesis, "_eligible_namespaces", return_value=[ns_row]), \
             patch.object(synthesis, "_due_for_synthesis", return_value=False), \
             patch.object(synthesis, "_load_facts_for_synthesis") as load_facts, \
             patch.object(synthesis, "_call_deepseek") as call_deepseek:
            written = run_synthesis_cycle(MagicMock())

        self.assertEqual(written, 0)
        load_facts.assert_not_called()
        call_deepseek.assert_not_called()

    def test_deepseek_failure_does_not_raise(self):
        namespace_id = uuid4()
        ns_row = {"namespace_id": namespace_id, "tenant_id": uuid4(), "acting_principal_id": uuid4(), "fact_count": 25}

        with patch.dict(os.environ, {"DEEPSEEK_API_KEY": "sk-test"}, clear=False), \
             patch.object(synthesis, "_eligible_namespaces", return_value=[ns_row]), \
             patch.object(synthesis, "_due_for_synthesis", return_value=True), \
             patch.object(synthesis, "_load_existing_patterns", return_value=[]), \
             patch.object(synthesis, "_load_facts_for_synthesis", return_value=[{"id": str(uuid4()), "content": "a"}]), \
             patch.object(synthesis, "_call_deepseek", side_effect=ValueError("boom")), \
             patch.object(synthesis, "_mark_synthesis_run") as mark_run:
            written = run_synthesis_cycle(MagicMock())

        self.assertEqual(written, 0)
        mark_run.assert_not_called()

    def test_a_pattern_already_known_is_not_proposed_again(self):
        namespace_id = uuid4()
        fact_a, fact_b = str(uuid4()), str(uuid4())
        ns_row = {"namespace_id": namespace_id, "tenant_id": uuid4(), "acting_principal_id": uuid4(), "fact_count": 25}
        with patch.dict(os.environ, {"DEEPSEEK_API_KEY": "sk-test"}, clear=False), \
             patch.object(synthesis, "_eligible_namespaces", return_value=[ns_row]), \
             patch.object(synthesis, "_due_for_synthesis", return_value=True), \
             patch.object(synthesis, "_load_existing_patterns", return_value=["Deploys cluster on Fridays across all services."]), \
             patch.object(synthesis, "_load_facts_for_synthesis",
                   return_value=[{"id": fact_a, "content": "a"}, {"id": fact_b, "content": "b"}]), \
             patch.object(synthesis, "_call_deepseek", return_value={
                 "patterns": [{"content": "Deploys cluster on Fridays across all services", "source_fact_ids": [fact_a, fact_b], "confidence": 0.8}]
             }), \
             patch.object(synthesis, "_propose_pattern") as propose, \
             patch.object(synthesis, "_mark_synthesis_run"):
            self.assertEqual(run_synthesis_cycle(MagicMock()), 0)
        propose.assert_not_called()

    def test_a_pattern_takes_its_sources_highest_sensitivity(self):
        self.assertEqual(synthesis.pattern_sensitivity([{"sensitivity": "internal"}, {"sensitivity": "confidential"}]), "confidential")
        self.assertEqual(synthesis.pattern_sensitivity([{"sensitivity": "public"}]), "internal")

    def test_proposal_is_a_pending_review_candidate(self):
        conn = MagicMock()
        conn.cursor.return_value.__enter__.return_value.fetchone.return_value = (1,)
        synthesis._propose_pattern(conn, uuid4(), uuid4(), uuid4(), "A pattern", [{"id": "f1", "sensitivity": "confidential"}, {"id": "f2"}], 0.7)
        calls = conn.cursor.return_value.__enter__.return_value.execute.call_args_list
        insert = next(c for c in calls if "INSERT INTO memory_candidates" in c.args[0])
        self.assertIn("'pending_review'", insert.args[0])
        self.assertEqual(insert.args[1][7], "confidential")
        self.assertTrue(any("INSERT INTO memory_items" in c.args[0] for c in calls) is False)


if __name__ == "__main__":
    unittest.main()
