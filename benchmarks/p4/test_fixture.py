import copy
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

from benchmarks.p4.offline_gate import load_fixture

FIXTURE = pathlib.Path(__file__).with_name("retrieval_fixture.json")


class P4FixtureTests(unittest.TestCase):
    def test_fixture_references_are_closed_and_unique(self):
        data = json.loads(FIXTURE.read_text())
        corpus_ids = [item["id"] for item in data["corpus"]]
        self.assertEqual(len(corpus_ids), len(set(corpus_ids)))
        case_ids = [case["id"] for case in data["cases"]]
        self.assertEqual(len(case_ids), len(set(case_ids)))
        known = set(corpus_ids)
        for case in data["cases"]:
            self.assertTrue(case["expected_ids"], case["id"])
            self.assertTrue(set(case["expected_ids"]) <= known, case["id"])
            self.assertTrue(set(case.get("forbidden_ids", [])) <= known, case["id"])
            self.assertFalse(set(case["expected_ids"]) & set(case.get("forbidden_ids", [])), case["id"])

    def test_fixture_contains_exact_semantic_temporal_and_mixed_cases(self):
        data = json.loads(FIXTURE.read_text())
        modes = {case["expected_mode"] for case in data["cases"]}
        self.assertTrue({"memory", "temporal", "exact_navigation", "code", "mixed"} <= modes)
        self.assertTrue(any(case.get("requires_semantic") for case in data["cases"]))

    def test_rejects_corpus_documents_without_searchable_content(self):
        source = json.loads(FIXTURE.read_text())
        for fields in ({}, {"text": " \t "}, {"fqn": "", "path": "", "text": ""}):
            with self.subTest(fields=fields), tempfile.TemporaryDirectory() as directory:
                altered = copy.deepcopy(source)
                document_id = altered["corpus"][0]["id"]
                altered["corpus"][0] = {"id": document_id, **fields}
                fixture = pathlib.Path(directory) / "fixture.json"
                fixture.write_text(json.dumps(altered))
                with self.assertRaisesRegex(ValueError, "searchable content"):
                    load_fixture(fixture)

    def test_fqn_only_document_is_valid_searchable_content(self):
        source = json.loads(FIXTURE.read_text())
        source["corpus"][0] = {"id": source["corpus"][0]["id"], "fqn": "auth.ControlPlaneHandler"}
        with tempfile.TemporaryDirectory() as directory:
            fixture = pathlib.Path(directory) / "fixture.json"
            fixture.write_text(json.dumps(source))
            loaded, _ = load_fixture(fixture)
            self.assertEqual(loaded["corpus"][0]["fqn"], "auth.ControlPlaneHandler")

    def test_cli_refuses_to_record_contentless_corpus(self):
        source = json.loads(FIXTURE.read_text())
        source["corpus"][0] = {"id": source["corpus"][0]["id"]}
        with tempfile.TemporaryDirectory() as directory:
            fixture = pathlib.Path(directory) / "fixture.json"
            baseline = pathlib.Path(directory) / "baseline.json"
            fixture.write_text(json.dumps(source))
            result = subprocess.run(
                [sys.executable, "-m", "benchmarks.p4.offline_gate",
                 "--fixture", str(fixture), "--record-baseline", str(baseline)],
                text=True, capture_output=True, check=False, timeout=30,
            )
            self.assertEqual(result.returncode, 2, result.stderr)
            self.assertIn("searchable content", result.stderr)
            self.assertFalse(baseline.exists())


if __name__ == "__main__":
    unittest.main()
