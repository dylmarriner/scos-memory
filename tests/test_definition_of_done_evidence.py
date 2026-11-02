"""Regression checks for the §41 evidence pointers, not a production acceptance claim."""
import ast
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
                doc.write_text("| § | Topic | Status | Basis |\n|---|---|---|---|\n| 41 | Definition of done | PARTIAL | still open |\n")
                self.assertTrue(dod.audit_doc_gate())
                doc.write_text("| § | Topic | Status | Basis |\n|---|---|---|---|\n| 41 | Definition of done | PASS | verified |\n")
                self.assertFalse(dod.audit_doc_gate())

    def test_a_comment_does_not_count_as_a_test_function(self):
        item = next(i for i in dod.ITEMS if i.bullet == "Backups have been restored successfully.")
        with TemporaryDirectory() as tmp:
            p = Path(tmp) / "evidence_test.go"
            p.write_text("// func TestBackupAndDrillsAgainstPostgreSQL(t *testing.T) {}\n")
            self.assertFalse(dod.function_exists(p, item.pattern))


    def test_go_test_names_inside_raw_strings_and_block_comments_are_not_evidence(self):
        item = next(i for i in dod.ITEMS if i.bullet == "Backups have been restored successfully.")
        with TemporaryDirectory() as tmp:
            path = Path(tmp) / "fake_test.go"
            path.write_text(
                'package fake\n'
                'var description = `\nfunc TestBackupAndDrillsAgainstPostgreSQL(t *testing.T) {}\n`\n'
                '/*\nfunc TestBackupAndDrillsAgainstPostgreSQL(t *testing.T) {}\n*/\n'
            )
            self.assertFalse(dod.function_exists(path, item.pattern))
            path.write_text(path.read_text() + '\nfunc TestBackupAndDrillsAgainstPostgreSQL(t *testing.T) {}\n')
            self.assertTrue(dod.function_exists(path, item.pattern))

    def test_python_test_names_inside_multiline_strings_are_not_evidence(self):
        item = next(i for i in dod.ITEMS if i.bullet == "Corrections preserve versions.")
        with TemporaryDirectory() as tmp:
            path = Path(tmp) / "fake_test.py"
            path.write_text(
                'class TestPromoter:\n'
                '    note = """\n'
                '    def test_build_memory_version_contains_version_1(self): pass\n'
                '    """\n'
            )
            ast.parse(path.read_text())  # Valid Python, not a syntax-error false negative.
            self.assertFalse(dod.function_exists(path, item.pattern))
            path.write_text('class TestPromoter:\n    def test_build_memory_version_contains_version_1(self): pass\n')
            self.assertTrue(dod.function_exists(path, item.pattern))

    def test_audit_does_not_accept_qualified_or_misleading_pass_status(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "docs" / "governance-audit-current.md"
            doc.parent.mkdir(parents=True)
            with patch.object(dod, "ROOT", root):
                for status in ("PASS (Carried)", "PASS pending deployment", "PASS-UNVERIFIED"):
                    with self.subTest(status=status):
                        doc.write_text(f"| § | Topic | Status | Basis |\n|---|---|---|---|\n| 41 | Definition of done | {status} | unresolved |\n")
                        self.assertTrue(dod.audit_doc_gate())
                doc.write_text("| § | Topic | Status | Basis |\n|---|---|---|---|\n| 41 | Definition of done | PASS | verified |\n")
                self.assertFalse(dod.audit_doc_gate())


    def test_forged_pass_before_master_table_cannot_override_real_partial(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "docs" / "governance-audit-current.md"
            doc.parent.mkdir(parents=True)
            doc.write_text(
                "| 41 | Definition of done | PASS | fabricated outside master table |\n"
                "\n| § | Topic | Status | Basis |\n"
                "|---|---|---|---|\n"
                "| 41 | Definition of done | PARTIAL (Carried) | outstanding gates |\n"
            )
            with patch.object(dod, "ROOT", root):
                self.assertTrue(dod.audit_doc_gate())

    def test_fenced_example_and_missing_master_table_fail_closed(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "docs" / "governance-audit-current.md"
            doc.parent.mkdir(parents=True)
            doc.write_text(
                "```markdown\n| § | Topic | Status | Basis |\n"
                "|---|---|---|---|\n"
                "| 41 | Definition of done | PASS | example only |\n```\n"
            )
            with patch.object(dod, "ROOT", root):
                self.assertTrue(dod.audit_doc_gate())

    def test_duplicate_master_table_rows_fail_closed(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "docs" / "governance-audit-current.md"
            doc.parent.mkdir(parents=True)
            doc.write_text(
                "| § | Topic | Status | Basis |\n"
                "|---|---|---|---|\n"
                "| 41 | Definition of done | PASS | first |\n"
                "| 41 | Definition of done | PARTIAL | second |\n"
            )
            with patch.object(dod, "ROOT", root):
                self.assertTrue(dod.audit_doc_gate())


    def test_fenced_example_does_not_override_valid_master_pass(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "docs" / "governance-audit-current.md"
            doc.parent.mkdir(parents=True)
            doc.write_text(
                "\x60\x60\x60markdown\n| § | Topic | Status | Basis |\n"
                "|---|---|---|---|\n"
                "| 41 | Definition of done | PARTIAL | historical example |\n"
                "\x60\x60\x60\n"
                "| § | Topic | Status | Basis |\n"
                "|---|---|---|---|\n"
                "| 40 | Delivery phases | PASS | completed |\n"
                "| 41 | Definition of done | PASS | verified |\n"
            )
            with patch.object(dod, "ROOT", root):
                self.assertFalse(dod.audit_doc_gate())

    def test_multiple_master_tables_or_wrong_section_topic_fail_closed(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "docs" / "governance-audit-current.md"
            doc.parent.mkdir(parents=True)
            table = (
                "| § | Topic | Status | Basis |\n"
                "|---|---|---|---|\n"
                "| 41 | Definition of done | PASS | verified |\n"
            )
            with patch.object(dod, "ROOT", root):
                doc.write_text(table + "\n" + table)
                self.assertTrue(dod.audit_doc_gate())
                doc.write_text(table.replace("Definition of done", "Other topic"))
                self.assertTrue(dod.audit_doc_gate())


    def test_html_comment_cannot_forge_master_table_pass(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "docs" / "governance-audit-current.md"
            doc.parent.mkdir(parents=True)
            doc.write_text(
                "<!--\n| § | Topic | Status | Basis |\n"
                "|---|---|---|---|\n"
                "| 41 | Definition of done | PASS | commented out |\n-->\n"
            )
            with patch.object(dod, "ROOT", root):
                self.assertTrue(dod.audit_doc_gate())


    def test_indented_code_example_cannot_forge_master_table_pass(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "docs" / "governance-audit-current.md"
            doc.parent.mkdir(parents=True)
            doc.write_text(
                "    | § | Topic | Status | Basis |\n"
                "    |---|---|---|---|\n"
                "    | 41 | Definition of done | PASS | indented example |\n"
            )
            with patch.object(dod, "ROOT", root):
                self.assertTrue(dod.audit_doc_gate())

    def test_extra_columns_in_section_41_row_fail_closed(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            doc = root / "docs" / "governance-audit-current.md"
            doc.parent.mkdir(parents=True)
            doc.write_text(
                "| § | Topic | Status | Basis |\n"
                "|---|---|---|---|\n"
                "| 41 | Definition of done | PASS | verified | conflicting basis |\n"
            )
            with patch.object(dod, "ROOT", root):
                self.assertTrue(dod.audit_doc_gate())


if __name__ == "__main__":
    unittest.main()
