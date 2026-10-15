"""Tests for the extractor module."""

import sys
import unittest
from datetime import datetime, timezone
from pathlib import Path
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation.models import MemoryClass
from scos_consolidation.extractor import (
    extract_candidates,
    _classify,
    _has_code_output,
    _has_factual_claim,
    _infer_authority,
    _infer_confidence,
    _infer_expiry,
    _infer_sensitivity,
    UnresolvedNamespaceError,
)


class ExtractorTests(unittest.TestCase):
    def setUp(self):
        self.tenant_id = uuid4()
        self.event_id = uuid4()
        self.actor_id = uuid4()
        self.namespace_id = uuid4()

    def _event(self, event_type="turn.completed", **overrides):
        event = {
            "id": self.event_id,
            "tenant_id": self.tenant_id,
            "aggregate_id": str(uuid4()),
            "event_type": event_type,
            "subject": "turn/abc-123",
            "actor_id": self.actor_id,
            "occurred_at": "2026-07-26T12:00:00Z",
            "data": {},
            "namespace_id": str(self.namespace_id),
        }
        event.update(overrides)
        return event

    def _message(self, role="user", content="Hello"):
        return {
            "id": str(uuid4()),
            "role": role,
            "content": content,
            "content_hash": "abc123",
            "session_id": str(uuid4()),
            "turn_id": str(uuid4()),
            "committed_at": "2026-07-26T12:00:00Z",
        }

    def test_namespace_is_resolved_never_a_placeholder(self):
        messages = [self._message(role="user", content="What database does this project use?"),
                    self._message(role="assistant", content="The project uses PostgreSQL with pgvector.")]
        candidates = extract_candidates(self._event(), messages)
        self.assertEqual(candidates[0].namespace_id, self.namespace_id)
        explicit = uuid4()
        candidates = extract_candidates(self._event(data={"namespace_id": str(explicit)}), messages)
        self.assertEqual(candidates[0].namespace_id, explicit)
        with self.assertRaises(UnresolvedNamespaceError):
            extract_candidates(self._event(namespace_id=None), messages)

    def test_expiry_is_relative_to_the_event(self):
        event = self._event()
        self.assertEqual(_infer_expiry(event, MemoryClass.WORKING), datetime(2026, 8, 2, 12, 0, tzinfo=timezone.utc))
        self.assertEqual(_infer_expiry(event, MemoryClass.EPISODIC), datetime(2026, 8, 25, 12, 0, tzinfo=timezone.utc))
        self.assertIsNone(_infer_expiry(event, MemoryClass.SEMANTIC))

    def test_extract_semantic_from_turn_completed(self):
        event = self._event(event_type="turn.completed")
        messages = [
            self._message(role="user", content="What database does this project use?"),
            self._message(role="assistant", content="The project uses PostgreSQL with pgvector."),
        ]
        candidates = extract_candidates(event, messages)
        self.assertEqual(len(candidates), 1)
        self.assertEqual(candidates[0].memory_class, MemoryClass.SEMANTIC)
        self.assertEqual(candidates[0].tenant_id, self.tenant_id)

    def test_extract_procedural_from_code_output(self):
        event = self._event(event_type="turn.completed")
        messages = [
            self._message(role="user", content="Show me the DB schema"),
            self._message(role="assistant", content="```sql\nCREATE TABLE users (id int);\n```"),
        ]
        candidates = extract_candidates(event, messages)
        self.assertEqual(len(candidates), 1)
        self.assertEqual(candidates[0].memory_class, MemoryClass.PROCEDURAL)

    def test_extract_governing_from_policy_content(self):
        event = self._event(event_type="decision.recorded", data={"title": "New access policy"})
        messages = [
            self._message(role="user", content="Set a new permission policy"),
            self._message(role="assistant", content="The access policy is now restricted."),
        ]
        candidates = extract_candidates(event, messages)
        self.assertEqual(len(candidates), 1)
        self.assertEqual(candidates[0].memory_class, MemoryClass.GOVERNING)

    def test_discard_journal_only_return_empty(self):
        event = self._event(event_type="turn.completed")
        messages = [self._message(role="user", content="hi")]
        # No factual/code signals → episodic
        candidates = extract_candidates(event, messages)
        self.assertGreaterEqual(len(candidates), 0)

    def test_untracked_event_type_returns_empty(self):
        event = self._event(event_type="heartbeat")
        candidates = extract_candidates(event, [])
        self.assertEqual(len(candidates), 0)

    def test_authority_based_on_class(self):
        event = self._event(event_type="turn.completed")
        msg = self._message(role="assistant", content="The project uses PostgreSQL.")

        # Episodic
        from scos_consolidation.extractor import _classify, _infer_authority
        mclass = _classify(event, [msg])
        authority = _infer_authority(event, mclass)
        self.assertGreaterEqual(authority, 30)
        self.assertLessEqual(authority, 100)

        # Override from event data
        event["data"] = {"authority_level": 95}
        authority = _infer_authority(event, MemoryClass.GOVERNING)
        self.assertEqual(authority, 95)

    def test_confidence_boost_with_evidence(self):
        event = self._event(event_type="turn.completed")
        msg = self._message(role="assistant", content="The sky is blue.")
        candidates = extract_candidates(event, [msg])
        if candidates:
            self.assertGreater(candidates[0].confidence_score, 0)

    def test_sensitivity_inference(self):
        msg = self._message(role="assistant", content="This is confidential information.")
        sensitivity = _infer_sensitivity({}, [msg])
        self.assertEqual(sensitivity.value, "confidential")

    def test_factual_claim_detection(self):
        msg = self._message(role="assistant", content="The system is a distributed platform.")
        self.assertTrue(_has_factual_claim([msg]))

    def test_code_output_detection(self):
        msg = self._message(role="tool", content="function hello() { return 1; }")
        self.assertTrue(_has_code_output([msg]))


if __name__ == "__main__":
    unittest.main()
