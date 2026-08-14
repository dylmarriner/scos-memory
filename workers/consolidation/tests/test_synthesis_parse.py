"""Unit tests for the LLM-JSON tolerant parser added to synthesis.py."""

import sys, os, unittest
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

# Avoid importing psycopg-dependent module paths that may not be present.
from scos_consolidation import synthesis  # noqa: E402


class ParseLLMJsonTests(unittest.TestCase):
    def test_plain_json(self):
        self.assertEqual(synthesis._parse_llm_json('{"patterns": []}'), {"patterns": []})

    def test_fenced_json(self):
        raw = '```json\n{"patterns": [{"content": "x", "source_fact_ids": ["a"], "confidence": 0.7}]}\n```'
        out = synthesis._parse_llm_json(raw)
        self.assertEqual(len(out["patterns"]), 1)

    def test_prose_wrapped(self):
        out = synthesis._parse_llm_json('Here you go: {"patterns": []} Regards')
        self.assertEqual(out, {"patterns": []})

    def test_truncated_tail_salvaged(self):
        out = synthesis._parse_llm_json('{"patterns": [{"content": "x"}]}]}')
        self.assertEqual(len(out["patterns"]), 1)

    def test_unparseable_raises(self):
        with self.assertRaises(ValueError):
            synthesis._parse_llm_json("no json at all here")


if __name__ == "__main__":
    unittest.main()
