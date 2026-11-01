import base64
import hashlib
import hmac
import importlib.util
import json
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

    def test_signed_probe_bearer_has_future_expiry_and_valid_signature(self):
        with patch.object(runner.time, "time", return_value=1000):
            token = runner.build_claims_token("test-secret", "tenant", "principal")
        header, payload, signature = token.split(".")
        expected = hmac.new(b"test-secret", f"{header}.{payload}".encode(), hashlib.sha256).digest()
        actual = base64.urlsafe_b64decode(signature + "=" * (-len(signature) % 4))
        self.assertTrue(hmac.compare_digest(actual, expected))
        claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
        self.assertEqual(claims["tenant_id"], "tenant")
        self.assertEqual(claims["principal_id"], "principal")
        self.assertGreater(claims["exp"], 1000)
        self.assertLessEqual(claims["exp"], 1900)

    def test_claims_bearer_renews_after_long_running_probe(self):
        with patch.dict(runner.os.environ, {"SCOS_SECURITY_ISSUER_SECRET": "test-secret"}):
            with patch.object(runner.FailureInjector, "_preflight", return_value=""):
                injector = runner.FailureInjector()
        with patch.object(runner.time, "time", return_value=1000):
            first = injector._headers()["Authorization"]
        with patch.object(runner.time, "time", return_value=2000):
            second = injector._headers()["Authorization"]
        self.assertNotEqual(first, second)
        for token, now in ((first, 1000), (second, 2000)):
            self.assertTrue(token.startswith("Bearer "))
            payload = token.removeprefix("Bearer ").split(".")[1]
            claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
            self.assertGreater(claims["exp"], now)

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
