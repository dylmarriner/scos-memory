import json
import re
import subprocess
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).parents[1]


class FrozenBenchmarkTests(unittest.TestCase):
    def test_structural_gate_passes(self):
        result = subprocess.run([sys.executable, "benchmarks/verify_frozen.py"], cwd=ROOT, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_manifest_is_explicit_about_live_execution(self):
        manifest = json.loads((ROOT / "benchmarks/corpora/v1/manifest.json").read_text())
        self.assertTrue(all(b.get("deterministic") for b in manifest["baselines"]))
        self.assertTrue(all(b.get("runtime_required", False) for b in manifest["baselines"] if b["name"] in {"vector", "rrf"}))

    def test_impact_queries_return_the_relevant_frozen_record(self):
        corpus = json.loads((ROOT / "benchmarks/corpora/v1/impact.json").read_text())
        records = corpus["records"]

        def tokens(value):
            return set(re.findall(r"[a-z0-9]+", value.lower()))

        for query in corpus["queries"]:
            query_tokens = tokens(query["query"])
            ranked = sorted(
                records,
                key=lambda record: len(query_tokens & tokens(record["text"])),
                reverse=True,
            )
            self.assertEqual(ranked[0]["id"], query["relevant"][0], query["id"])


if __name__ == "__main__":
    unittest.main()
