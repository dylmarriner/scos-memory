"""Regression checks for the §41 evidence pointers, not a production acceptance claim."""
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest.mock import patch

from scripts import verify_definition_of_done as dod


class DefinitionOfDoneEvidenceTests(unittest.TestCase):
    def test_rebuild_and_restore_evidence_points_to_real_integration_tests(self):
        expected = {
            "Search indexes rebuild from authority.": (
                "services/code-intelligence/cmd/server/index_equivalence_integration_test.go",
                "TestPGIncrementalSnapshotEquivalentToCleanRebuild",
            ),
            "Backups have been restored successfully.": (
                "services/backup-manager/cmd/server/backup_integration_test.go",
                "TestBackupAndDrillsAgainstPostgreSQL",
            ),
        }
        for bullet, (path, name) in expected.items():
            with self.subTest(bullet=bullet):
                item = next(i for i in dod.ITEMS if i.bullet == bullet)
                self.assertEqual(path, item.file)
                self.assertIn(name, item.pattern)
                self.assertTrue(dod.function_exists(dod.ROOT / item.file, item.pattern))

    def test_missing_real_evidence_is_still_a_failure(self):
        with TemporaryDirectory() as tmp:
            with patch.object(dod, "ROOT", Path(tmp)):
                failures, _ = dod.check_items()
        self.assertTrue(any("Search indexes rebuild" in f for f in failures))
        self.assertTrue(any("Backups have been restored" in f for f in failures))

    def test_audit_respects_documented_partial_and_future_pass(self):
        # The gate must reject a partial audit but must not block a future,
        # evidence-backed PASS simply because this test was written earlier.
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "docs" / "governance-audit-current.md"
            doc.parent.mkdir(parents=True)
            with patch.object(dod, "ROOT", root):
                doc.write_text("| 41 | Definition of done | PARTIAL | still open |\n")
                self.assertTrue(dod.audit_doc_gate())
                doc.write_text("| 41 | Definition of done | PASS | verified |\n")
                self.assertFalse(dod.audit_doc_gate())

    def test_a_comment_does_not_count_as_a_test_function(self):
        item = next(i for i in dod.ITEMS if i.bullet == "Backups have been restored successfully.")
        with TemporaryDirectory() as tmp:
            p = Path(tmp) / "evidence_test.go"
            p.write_text("// func TestBackupAndDrillsAgainstPostgreSQL(t *testing.T) {}\n")
            self.assertFalse(dod.function_exists(p, item.pattern))


if __name__ == "__main__":
    unittest.main()
