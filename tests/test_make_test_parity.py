"""Keep the documented local test entrypoint aligned with CI's Python gates.

This uses make -n instead of parsing the Makefile directly so it catches
recipe expansions and verifies that a developer invoking make test actually
runs the same no-external-service safety regressions as CI.
"""
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[1]
CI = ROOT / ".github/workflows/ci.yml"

# These CI checks guard benchmark evidence, provenance, RLS coverage and
# governance. A green local make test must not quietly skip them.
REQUIRED_LOCAL_CHECKS = (
    "packages/benchmark-harness/python/tests",
    "test_benchmark_ranking_uniqueness.py",
    "test_benchmark_harness.py",
    "test_benchmark_suite_schema.py",
    "scripts/test_scos_verify_snapshot_manifest.py",
    "scripts/test_scos_verify_snapshot_events.py",
    "scripts/test_scos_verify_snapshot_bundle.py",
    "test_p6_rls_source_evidence.py",
    "test_p7_integration_ci_gate.py",
    "tests/test_ci_supply_chain.py",
    "scripts/validate_json.py",
    "scripts/audit_governance.py",
)


class MakeTestParityTests(unittest.TestCase):
    def test_local_test_target_keeps_ci_python_integrity_gates(self):
        workflow = CI.read_text(encoding="utf-8")
        proc = subprocess.run(
            ["make", "--no-print-directory", "-n", "test"],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=True,
        )
        for gate in REQUIRED_LOCAL_CHECKS:
            with self.subTest(gate=gate):
                self.assertIn(gate, workflow, f"CI gate unexpectedly removed: {gate}")
                self.assertIn(gate, proc.stdout, f"make test skips a required CI gate: {gate}")


if __name__ == "__main__":
    unittest.main()
