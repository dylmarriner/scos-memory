import importlib.util
import pathlib
import tempfile
import unittest


ROOT = pathlib.Path(__file__).parents[1]
SCRIPT = ROOT / "scripts/audit_governance_runtime.py"
SPEC = importlib.util.spec_from_file_location("audit_governance_runtime", SCRIPT)
audit_runtime = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(audit_runtime)


class RuntimeGovernanceAuditTests(unittest.TestCase):
    def test_repository_reports_known_runtime_findings(self):
        findings = audit_runtime.audit(ROOT)

        self.assertFalse(any("HTTP 501/unavailable" in finding for finding in findings))
        self.assertFalse(any("local JSON authority fallback" in finding for finding in findings))
        self.assertFalse(any("unconditionally healthy" in finding for finding in findings))
        self.assertFalse(any("without authentication middleware" in finding for finding in findings))
        self.assertEqual(
            [finding for finding in findings if "failure-injection" in finding],
            [],
        )
        self.assertFalse(any("benchmark evidence is absent" in finding for finding in findings))

    def test_clean_fixture_passes_each_runtime_gate(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "services/example/cmd/server").mkdir(parents=True)
            (root / "tests/failure-injection").mkdir(parents=True)
            (root / "benchmarks").mkdir()
            (root / "services/example/cmd/server/main.go").write_text(
                "package main\n"
                "func main() { mux := authMiddleware(http.NewServeMux()); _ = mux }\n",
                encoding="utf-8",
            )
            (root / "tests/failure-injection/runner.py").write_text(
                "def run():\n    return 'executed'\n", encoding="utf-8"
            )
            (root / "benchmarks/acceptance-results.json").write_text(
                '{"baseline": "frozen", "results": [{"case": "1"}], '
                '"comparisons": {}, "confidence_intervals": {}, '
                '"task_success": {}, "hardware": "test"}\n', encoding="utf-8"
            )

            self.assertEqual(audit_runtime.audit(root), [])

    def test_control_plane_handler_counts_as_authentication_middleware(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "services/example/cmd/server").mkdir(parents=True)
            (root / "tests/failure-injection").mkdir(parents=True)
            (root / "benchmarks").mkdir()
            (root / "services/example/cmd/server/main.go").write_text(
                "package main\n"
                "func main() { http.ListenAndServe(\":8080\", auth.ControlPlaneHandler(mux, config)) }\n",
                encoding="utf-8",
            )
            (root / "tests/failure-injection/runner.py").write_text(
                "def run():\n    return 'executed'\n", encoding="utf-8"
            )
            (root / "benchmarks/acceptance-results.json").write_text(
                '{"baseline": "frozen", "results": [{"case": "1"}], '
                '"comparisons": {}, "confidence_intervals": {}, '
                '"task_success": {}, "hardware": "test"}\n', encoding="utf-8"
            )

            findings = audit_runtime.audit(root)
            self.assertFalse(any("without authentication middleware" in finding for finding in findings))

    def test_an_unwrapped_tlsserve_server_is_reported(self):
        # Every fabric service starts through tlsserve.ListenAndServe; a check
        # that only matched http.ListenAndServe could never fire.
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            path = root / "services/open/cmd/server/main.go"
            path.parent.mkdir(parents=True)
            path.write_text(
                "package main\n"
                "func main() {\n"
                "\tmux.HandleFunc(\"/v1/things\", handle)\n"
                "\ttlsserve.ListenAndServe(addr, telemetry.Instrument(\"open\", mux))\n"
                "}\n",
                encoding="utf-8",
            )
            self.assertIn(
                "services/open/cmd/server/main.go:4: HTTP server exposes /v1 routes without authentication middleware",
                audit_runtime.audit(root),
            )

    def test_fixture_finds_exact_501_and_readiness_lines(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            path = root / "services/demo/cmd/server/main.go"
            path.parent.mkdir(parents=True)
            path.write_text(
                "package main\n"
                "func handler(w http.ResponseWriter) { w.WriteHeader(http.StatusNotImplemented) }\n"
                "func main() { mux.HandleFunc(\"/health/ready\", healthOK) }\n",
                encoding="utf-8",
            )

            findings = audit_runtime.audit(root)
            self.assertIn(
                "services/demo/cmd/server/main.go:2: runtime handler returns HTTP 501/unavailable",
                findings,
            )
            self.assertIn(
                "services/demo/cmd/server/main.go:3: readiness endpoint is unconditionally healthy",
                findings,
            )


if __name__ == "__main__":
    unittest.main()
