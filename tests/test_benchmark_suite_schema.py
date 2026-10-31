"""Fail-closed validation of externally supplied P0 benchmark suite evidence."""
import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
from benchmarks.harness import load_suite


class SuiteSchemaIntegrityTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = pathlib.Path(self.temp.name) / "suite.json"

    def base(self):
        return {
            "name": "schema-gate", "version": "1",
            "corpus": {"doc": "actual evidence"},
            "cases": [{
                "id": "case", "query": "actual evidence", "category": "known_answer",
                "expected_ids": ["doc"],
            }],
        }

    def load(self, payload):
        self.path.write_text(json.dumps(payload), encoding="utf-8")
        return load_suite(str(self.path))

    def test_empty_ground_truth_must_not_report_perfect_task_success(self):
        payload = self.base()
        payload["cases"][0]["expected_ids"] = []
        with self.assertRaisesRegex(ValueError, "expected_ids"):
            self.load(payload)

    def test_ground_truth_must_be_nonempty_unique_string_list(self):
        for invalid in ("doc", [""], ["doc", "doc"], [1], [None], {"doc": True}):
            with self.subTest(value=invalid):
                payload = self.base()
                payload["cases"][0]["expected_ids"] = invalid
                with self.assertRaisesRegex(ValueError, "expected_ids"):
                    self.load(payload)

    def test_semantic_labels_must_be_unique_string_lists(self):
        for field in ("citation_ids", "authoritative_ids", "conflicting_ids",
                      "duplicate_ids", "stale_ids", "forbidden_ids"):
            for invalid in ("doc", ["doc", "doc"], [1], [None], [""]):
                with self.subTest(field=field, value=invalid):
                    payload = self.base()
                    payload["cases"][0][field] = invalid
                    with self.assertRaisesRegex(ValueError, field):
                        self.load(payload)

    def test_boolean_and_budget_cannot_be_coerced(self):
        for field, invalid in (("promotion_expected", "false"),
                               ("promotion_expected", 1),
                               ("token_budget", True),
                               ("token_budget", -1),
                               ("token_budget", 1.5),
                               ("context", "not an object"),
                               ("context", [])):
            with self.subTest(field=field, value=invalid):
                payload = self.base()
                payload["cases"][0][field] = invalid
                with self.assertRaisesRegex(ValueError, field):
                    self.load(payload)

    def test_corpus_and_case_identity_cannot_be_blank(self):
        for mutate, field in (
            (lambda p: p.update(name=" "), "name"),
            (lambda p: p.update(version=" "), "version"),
            (lambda p: p.update(corpus={" ": "evidence"}), "corpus"),
            (lambda p: p.update(corpus={"doc": " "}), "corpus"),
            (lambda p: p["cases"][0].update(id=" "), "id"),
            (lambda p: p["cases"][0].update(query=" "), "query"),
            (lambda p: p["cases"][0].update(category=" "), "category"),
        ):
            with self.subTest(field=field):
                payload = self.base()
                mutate(payload)
                with self.assertRaisesRegex(ValueError, field):
                    self.load(payload)

    def test_cases_must_be_nonempty_array_of_objects(self):
        for invalid in ({"case": "value"}, "not a list", [None], ["text"], []):
            with self.subTest(value=invalid):
                payload = self.base()
                payload["cases"] = invalid
                with self.assertRaisesRegex(ValueError, "cases"):
                    self.load(payload)

    def test_well_formed_optional_labels_and_context_still_work(self):
        payload = self.base()
        payload["cases"][0].update(
            context={"tenant": "t"}, citation_ids=["doc"], authoritative_ids=["doc"],
            conflicting_ids=["doc"], duplicate_ids=["doc"], stale_ids=["doc"],
            promotion_expected=False, token_budget=0,
        )
        suite = self.load(payload)
        self.assertEqual(suite.cases[0].expected_ids, ["doc"])
        self.assertFalse(suite.cases[0].promotion_expected)


if __name__ == "__main__":
    unittest.main()
