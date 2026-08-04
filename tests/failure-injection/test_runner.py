import importlib.util
import pathlib
import sys
import unittest
from unittest.mock import patch


RUNNER_PATH = pathlib.Path(__file__).with_name("runner.py")
SPEC = importlib.util.spec_from_file_location("failure_injection_runner", RUNNER_PATH)
runner = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = runner
SPEC.loader.exec_module(runner)


class FailureInjectorContractTests(unittest.TestCase):
    def test_matrix_has_58_cases(self):
        self.assertEqual(sum(len(cases) for cases in runner.FAILURE_CATEGORIES.values()), 58)

    def test_probe_has_no_local_fallback(self):
        source = RUNNER_PATH.read_text(encoding="utf-8")
        self.assertNotIn("run_local_failure_test", source)
        self.assertNotIn("_load_local_class", source)
        self.assertIn("docker", source)
        self.assertIn("urlopen", source)
        self.assertIn("ON CONFLICT", source)

    def test_absent_stack_fails_clearly(self):
        injector = runner.FailureInjector(target_url="http://127.0.0.1:1")
        result = injector.execute_category_test("infrastructure", "storage outage")
        self.assertEqual(result.status, "failed")
        self.assertTrue("stack" in result.detail.lower() or "token" in result.detail.lower())

    def test_missing_required_tokens_fails_clearly(self):
        with patch.dict(runner.os.environ, {name: "" for name in runner.REQUIRED_TOKENS}):
            injector = runner.FailureInjector(target_url="http://127.0.0.1:1")
        self.assertIn("SCOS_INTERNAL_SERVICE_TOKEN", injector.stack_error)
        self.assertIn("SCOS_CONTROL_PLANE_TOKEN", injector.stack_error)
        self.assertIn("SCOS_SECURITY_ISSUER_SECRET", injector.stack_error)


if __name__ == "__main__":
    unittest.main()
