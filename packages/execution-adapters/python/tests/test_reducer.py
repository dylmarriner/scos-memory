import json
import unittest
from pathlib import Path

from scos_execution.reducer import (RISK_DESTRUCTIVE, RISK_READ, RISK_TEST, RISK_WRITE, OutputReducer,
                                    classify_risk)


class OutputReducerTests(unittest.TestCase):
    def test_rewrite_blocks_destructive(self):
        red = OutputReducer()
        for command in ("rm -rf /", "ls; rm -rf /tmp/x", "git push origin main"):
            rw = red.rewrite_command(command)
            self.assertTrue(rw.is_rewritten, command)
            self.assertEqual(rw.rewritten_command, "")
        safe = red.rewrite_command("ls -la")
        self.assertFalse(safe.is_rewritten)
        with self.assertRaises(ValueError):
            red.rewrite_command("   ")

    def test_reduce_output(self):
        out = OutputReducer().reduce_output("x" * 5000, 100)
        self.assertIn("reduced", out)

    def test_classify_matches_shared_cases(self):
        # The same cases packages/execution-adapters/risk_test.go checks.
        cases = json.loads((Path(__file__).resolve().parents[2] / "risk_cases.json").read_text(encoding="utf-8"))["cases"]
        self.assertGreaterEqual(len(cases), 50)
        for command, want in cases.items():
            self.assertEqual(classify_risk(command), want, command)

if __name__ == "__main__":
    unittest.main()
