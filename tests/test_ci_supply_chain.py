"""Regression contract: security scans cannot pass by hiding vulnerable Go toolchains."""
import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
WORKFLOWS = (
    ".github/workflows/ci.yml",
    ".github/workflows/p4-hybrid-retrieval.yml",
    ".github/workflows/model-runtime.yml",
)
PATCHED_GO = "1.26.10"  # Go 2026-10-19 crypto/x509 security release.


class CISupplyChainTests(unittest.TestCase):
    def test_every_go_workflow_uses_patched_toolchain(self):
        for filename in WORKFLOWS:
            with self.subTest(workflow=filename):
                text = (ROOT / filename).read_text()
                versions = re.findall(r'go-version:\s*["\']([^"\']+)["\']', text)
                self.assertTrue(versions, "no Go setup-go version declared")
                self.assertTrue(all(version == PATCHED_GO for version in versions), versions)

    def test_module_toolchain_cannot_downgrade_to_vulnerable_patch(self):
        self.assertRegex((ROOT / "go.mod").read_text(), r"(?m)^toolchain go1\.26\.10$")

    def test_govulncheck_fails_closed_and_is_exercised_by_ci(self):
        text = (ROOT / WORKFLOWS[0]).read_text()
        scan = text.split("- name: Go vulnerability scan (govulncheck)", 1)[1].split("- uses: actions/setup-python", 1)[0]
        self.assertIn("govulncheck ./...", scan)
        self.assertNotIn("continue-on-error: true", scan)
        self.assertIn("python3 tests/test_ci_supply_chain.py -v", text)


if __name__ == "__main__":
    unittest.main()
