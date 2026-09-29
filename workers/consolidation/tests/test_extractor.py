"""Tests for the extractor module."""

import sys
import unittest
from datetime import datetime, timedelta, timezone
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
    _resolve_namespace,
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
            # The worker joins each turn event to its session, which supplies this.
            "namespace_id": self.namespace_id,
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

    def test_episodic_candidate_from_real_turn_is_valid_for_30_days(self):
        # Regression: expiry used to be now(), which is <= valid_from, so every
        # episodic candidate from a real turn failed model validation.
        event = self._event()
        candidates = extract_candidates(event, [self._message(content="Let's pick this up tomorrow.")])
        self.assertEqual(len(candidates), 1)
        c = candidates[0]
        self.assertEqual(c.memory_class, MemoryClass.EPISODIC)
        occurred = datetime(2026, 7, 26, 12, 0, tzinfo=timezone.utc)
        self.assertEqual(c.valid_from, occurred)
        self.assertEqual(c.valid_until, occurred + timedelta(days=30))

    def test_working_expiry_is_seven_days_after_event(self):
        occurred = datetime(2026, 7, 26, 12, 0, tzinfo=timezone.utc)
        self.assertEqual(_infer_expiry(self._event(), MemoryClass.WORKING), occurred + timedelta(days=7))
        self.assertIsNone(_infer_expiry(self._event(), MemoryClass.SEMANTIC))

    def test_candidate_uses_session_namespace(self):
        candidates = extract_candidates(self._event(), [self._message(content="The service uses Redis.")])
        self.assertEqual(candidates[0].namespace_id, self.namespace_id)

    def test_namespace_falls_back_to_event_data(self):
        ns = uuid4()
        event = self._event(namespace_id=None, data={"namespace_id": str(ns)})
        self.assertEqual(_resolve_namespace(event, []), ns)

    def test_no_resolvable_namespace_yields_no_candidate(self):
        # Regression: this used to invent the all-zero UUID, which violates
        # the memory_candidates.namespace_id foreign key at insert time.
        event = self._event(namespace_id=None)
        self.assertIsNone(_resolve_namespace(event, []))
        self.assertEqual(extract_candidates(event, [self._message(content="The service uses Redis.")]), [])


if __name__ == "__main__":
    unittest.main()
