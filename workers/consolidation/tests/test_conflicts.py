"""Tests for the conflict detection module."""

import importlib
import sys
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import MagicMock, patch
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation.models import MemoryCandidate, MemoryClass, SensitivityClass, EvidenceReference
from scos_consolidation.conflicts import ConflictSet, detect_conflicts


def _candidate(**overrides) -> MemoryCandidate:
    base = {
        "candidate_id": uuid4(),
        "tenant_id": uuid4(),
        "namespace_id": uuid4(),
        "source_event_id": uuid4(),
        "memory_class": MemoryClass.SEMANTIC,
        "title": "Knowledge: Database engine",
        "content": "The project uses PostgreSQL.",
        "authority_level": 70,
        "confidence_score": 0.85,
        "sensitivity_class": SensitivityClass.INTERNAL,
        "evidence": [EvidenceReference(kind="event", reference_id=str(uuid4()))],
    }
    base.update(overrides)
    return MemoryCandidate(**base)


class ConflictTests(unittest.TestCase):
    def test_negation_conflicts_whatever_the_titles(self):
        c = _candidate(title="Architecture note", content="The API is not a monolith. It runs as four services.")
        existing = [{"id": str(uuid4()), "kind": "memory", "title": "System shape", "content": "The API is a monolith.", "authority_level": 70}]
        conflicts = detect_conflicts([c], existing)
        self.assertEqual(len(conflicts), 1)
        self.assertIn("not a monolith", conflicts[0].statements[0])

    def test_agreeing_statements_do_not_conflict(self):
        c = _candidate(title="Architecture note", content="The API is a monolith.")
        existing = [{"id": str(uuid4()), "kind": "memory", "title": "System shape", "content": "The API is a monolith deployed on Fridays.", "authority_level": 70}]
        self.assertEqual(detect_conflicts([c], existing), [])

    def test_no_conflict_on_same_subject_same_claim(self):
        c = _candidate(title="Database engine", content="The project uses PostgreSQL.")
        existing = [
            {
                "id": str(uuid4()),
                "title": "Database engine",
                "content": "The project uses PostgreSQL.",
                "authority_level": 70,
            }
        ]
        conflicts = detect_conflicts([c], existing)
        self.assertEqual(len(conflicts), 0)

    def test_conflict_on_authority_mismatch(self):
        c = _candidate(title="Database engine", content="The project uses PostgreSQL.", authority_level=30)
        existing = [
            {
                "id": str(uuid4()),
                "title": "Database engine",
                "content": "The project uses PostgreSQL with MySQL.",
                "authority_level": 90,
            }
        ]
        conflicts = detect_conflicts([c], existing)
        # 90 - 30 = 60 >= 40 → authority mismatch conflict
        self.assertGreaterEqual(len(conflicts), 1)

    def test_no_conflict_different_subjects(self):
        c = _candidate(title="Database engine", content="PostgreSQL")
        existing = [
            {
                "id": str(uuid4()),
                "title": "Deployment target",
                "content": "The project deploys to Kubernetes.",
                "authority_level": 80,
            }
        ]
        conflicts = detect_conflicts([c], existing)
        self.assertEqual(len(conflicts), 0)

    def test_no_conflict_empty_existing(self):
        c = _candidate(title="Database engine", content="PostgreSQL")
        conflicts = detect_conflicts([c], [])
        self.assertEqual(len(conflicts), 0)

    def test_empty_candidates_no_conflicts(self):
        conflicts = detect_conflicts([], [{"id": str(uuid4()), "title": "x", "content": "y"}])
        self.assertEqual(len(conflicts), 0)

    def test_subjects_are_the_candidate_and_the_existing_record(self):
        c = _candidate(title="Database engine", content="The project uses PostgreSQL.", authority_level=30)
        memory_id, pending_id = str(uuid4()), str(uuid4())
        existing = [
            {"kind": "memory", "id": memory_id, "title": "Database engine",
             "content": "The project uses MySQL.", "authority_level": 90},
            {"kind": "candidate", "id": pending_id, "title": "Database engine",
             "content": "The project uses SQLite.", "authority_level": 90},
        ]
        conflicts = detect_conflicts([c], existing)
        self.assertEqual(len(conflicts), 2)
        self.assertEqual(conflicts[0].subjects, [str(c.candidate_id), memory_id])
        self.assertEqual(conflicts[0].subject_refs, [
            {"kind": "candidate", "id": str(c.candidate_id)}, {"kind": "memory", "id": memory_id}])
        self.assertEqual(conflicts[1].subject_refs[1], {"kind": "candidate", "id": pending_id})
        # The source event is evidence, never a subject: it is not a row a
        # conflict can be recorded on.
        for conflict in conflicts:
            self.assertNotIn(str(c.source_event_id), conflict.subjects)
            self.assertEqual(conflict.to_dict()["evidence"][0]["event_id"], str(c.source_event_id))

    def test_existing_row_without_id_is_not_a_conflict_subject(self):
        c = _candidate(title="Database engine", authority_level=30)
        existing = [{"title": "Database engine", "content": "The project uses MySQL.", "authority_level": 90}]
        self.assertEqual(detect_conflicts([c], existing), [])

    def test_conflict_set_rejects_unknown_subject_kinds(self):
        with self.assertRaises(ValueError):
            ConflictSet(uuid4(), [{"kind": "event", "id": str(uuid4())}], ["x"], datetime.now(timezone.utc), [])
        with self.assertRaises(ValueError):
            ConflictSet(uuid4(), [{"kind": "memory", "id": ""}], ["x"], datetime.now(timezone.utc), [])


class WriteConflictTests(unittest.TestCase):
    def _worker(self):
        patcher = patch.dict("sys.modules", {"psycopg": MagicMock(), "psycopg.rows": MagicMock(), "psycopg.sql": MagicMock()})
        patcher.start()
        self.addCleanup(patcher.stop)
        import scos_consolidation.worker as w
        importlib.reload(w)
        return w

    def _conflict(self, c, memory_id):
        existing = [{"kind": "memory", "id": memory_id, "title": "Database engine",
                     "content": "The project uses MySQL.", "authority_level": 90}]
        return detect_conflicts([c], existing)[0]

    def test_conflict_is_recorded_on_the_candidate_and_the_memory(self):
        w = self._worker()
        c = _candidate(title="Database engine", authority_level=30)
        memory_id = str(uuid4())
        conflict = self._conflict(c, memory_id)
        conn = MagicMock()
        cur = conn.cursor.return_value.__enter__.return_value
        cur.rowcount = 1
        w._write_conflict(conn, conflict)
        calls = cur.execute.call_args_list
        self.assertEqual(len(calls), 2)
        self.assertIs(calls[0].args[0], w._CONFLICT_TARGETS["candidate"])
        self.assertEqual(calls[0].args[1][1], str(c.candidate_id))
        self.assertIs(calls[1].args[0], w._CONFLICT_TARGETS["memory"])
        self.assertEqual(calls[1].args[1][1], memory_id)

    def test_a_subject_matching_no_row_fails_instead_of_vanishing(self):
        w = self._worker()
        conflict = self._conflict(_candidate(title="Database engine", authority_level=30), str(uuid4()))
        conn = MagicMock()
        conn.cursor.return_value.__enter__.return_value.rowcount = 0
        with self.assertRaises(RuntimeError):
            w._write_conflict(conn, conflict)


if __name__ == "__main__":
    unittest.main()
