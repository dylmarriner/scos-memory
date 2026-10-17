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

    def test_an_ordinary_turn_is_an_episodic_record(self):
        event = self._event(event_type="turn.completed")
        messages = [
            self._message(role="user", content="What database does this project use?"),
            self._message(role="assistant", content="The project uses PostgreSQL with pgvector."),
        ]
        candidates = extract_candidates(event, messages)
        self.assertEqual(len(candidates), 1)
        c = candidates[0]
        self.assertEqual(c.memory_class, MemoryClass.EPISODIC)
        self.assertEqual(c.confidence_score, 0.80)
        self.assertIsNotNone(c.valid_until)
        self.assertEqual(c.tenant_id, self.tenant_id)

    def test_keywords_alone_never_make_durable_knowledge(self):
        # A question mentioning "import", or a code block, is not a
        # procedure; "is a" or "uses" is not a fact.
        for user, assistant in [
            ("how do I import pandas in a notebook here", "Use import pandas as pd at the top of the notebook."),
            ("Show me the users table definition please", "```sql\nCREATE TABLE users (id int);\n```"),
            ("Tell me about the timezone in New Zealand today", "NZ uses NZST, which is a UTC+12 zone."),
        ]:
            candidates = extract_candidates(self._event(), [self._message("user", user), self._message("assistant", assistant)])
            self.assertTrue(all(c.memory_class == MemoryClass.EPISODIC for c in candidates), (user, candidates))

    def test_chit_chat_is_not_remembered(self):
        for user, assistant in [("thanks!", "You are welcome."), ("hi", "Hello there!")]:
            self.assertEqual(extract_candidates(self._event(), [self._message("user", user), self._message("assistant", assistant)]), [])

    def test_explicit_memory_request_stores_the_statement_not_the_transcript(self):
        messages = [
            self._message(role="user", content="Remember that our production database is PostgreSQL 17 on Linode."),
            self._message(role="assistant", content="Noted."),
        ]
        candidates = extract_candidates(self._event(), messages)
        self.assertEqual(len(candidates), 1)
        c = candidates[0]
        self.assertEqual(c.memory_class, MemoryClass.SEMANTIC)
        self.assertEqual(c.content, "Our production database is PostgreSQL 17 on Linode.")
        self.assertEqual(c.title, "Our production database is PostgreSQL 17 on Linode.")
        self.assertNotIn("[user]", c.content)

    def test_title_comes_from_content_not_the_event_subject(self):
        messages = [
            self._message(role="user", content="How do we roll back a failed deploy? Asking for the runbook."),
            self._message(role="assistant", content="Revert the release tag and redeploy the previous image."),
        ]
        candidates = extract_candidates(self._event(), messages)
        self.assertEqual(candidates[0].title, "How do we roll back a failed deploy?")
        self.assertFalse(candidates[0].title.startswith("turn/"))

    def test_a_declared_class_is_used(self):
        event = self._event(event_type="decision.recorded", data={"title": "New access policy", "memory_class": "governing"})
        messages = [
            self._message(role="user", content="Set a new permission policy"),
            self._message(role="assistant", content="The access policy is now restricted."),
        ]
        candidates = extract_candidates(event, messages)
        self.assertEqual(len(candidates), 1)
        self.assertEqual(candidates[0].memory_class, MemoryClass.GOVERNING)

    def test_declared_knowledge_event_types(self):
        messages = [self._message("user", "We settled the rollback procedure for failed deploys today."),
                    self._message("assistant", "Recorded: revert the release tag, then redeploy the previous image.")]
        self.assertEqual(extract_candidates(self._event(event_type="task.resolved"), messages)[0].memory_class, MemoryClass.PROCEDURAL)
        self.assertEqual(extract_candidates(self._event(event_type="decision.recorded"), messages)[0].memory_class, MemoryClass.SEMANTIC)

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

    def test_confidence_does_not_grow_with_evidence(self):
        few = [self._message("user", "Remember that the staging cluster runs in Sydney.")]
        many = few + [self._message("assistant", "Noted, staging is in Sydney.") for _ in range(6)]
        self.assertEqual(_infer_confidence(self._event(), MemoryClass.SEMANTIC, few), 0.90)
        self.assertEqual(_infer_confidence(self._event(), MemoryClass.SEMANTIC, many), 0.90)
        self.assertEqual(_infer_confidence(self._event(data={"confidence": 0.42}), MemoryClass.SEMANTIC, few), 0.42)

    def test_sensitivity_inference(self):
        msg = self._message(role="assistant", content="This is confidential information.")
        sensitivity = _infer_sensitivity({}, [msg])
        self.assertEqual(sensitivity.value, "confidential")

    def test_sensitivity_takes_the_most_sensitive_message(self):
        public_first = [self._message("user", "Our SDK is open source and public."),
                        self._message("assistant", "The signing details are confidential.")]
        self.assertEqual(_infer_sensitivity({}, public_first).value, "confidential")
        self.assertEqual(_infer_sensitivity({}, [self._message("user", "It is public on GitHub.")]).value, "internal")
        self.assertEqual(_infer_sensitivity({"data": {"sensitivity": "public"}}, [self._message("user", "Release notes for v2.")]).value, "public")

    def test_credentials_are_never_consolidated(self):
        messages = [self._message("user", "The database password is hunter2-prod please keep it handy for later"),
                    self._message("assistant", "Understood, I will keep the production password handy.")]
        self.assertEqual(extract_candidates(self._event(), messages), [])



if __name__ == "__main__":
    unittest.main()
