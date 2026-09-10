import json
import pathlib
import unittest

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

if __name__ == "__main__":
    unittest.main()
