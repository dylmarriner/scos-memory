"""Regression: successful HTTP JSON must not leak benchmark credentials."""
import io
import os
import pathlib
import tempfile
import threading
import sys
from contextlib import redirect_stdout
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import unittest
from unittest.mock import patch

from benchmarks.deployed import BenchmarkConfig, DeployedCodeClient
from benchmarks.run_deployed import main as run_deployed_main


class Response(io.BytesIO):
    status = 200


class CredentialReflectionTests(unittest.TestCase):
    def setUp(self):
        self.config = BenchmarkConfig(
            endpoint="https://example.test", token="secret-benchmark-token",
            control_plane_token="secret-control-plane-token",
            tenant_id="tenant-1", principal_id="principal-1",
        )
        self.client = DeployedCodeClient(self.config)

    def test_health_rejects_extra_debug_field_echoing_bearer(self):
        body = {"status": "ok", "debug": "Bearer secret-benchmark-token"}
        with patch.object(self.client.opener, "open", return_value=Response(json.dumps(body).encode())):
            with self.assertRaises(RuntimeError) as caught:
                self.client.health()
        self.assertNotIn(self.config.token, str(caught.exception))

    def test_query_rejects_control_plane_token_inside_nested_result(self):
        body = {"results": [{"Path": "safe.go", "extra": {"trace": "secret-control-plane-token"}}]}
        with patch.object(self.client.opener, "open", return_value=Response(json.dumps(body).encode())):
            with self.assertRaises(RuntimeError) as caught:
                self.client.query("repo", "main", "safe.go")
        self.assertNotIn(self.config.control_plane_token, str(caught.exception))

    def test_reflected_health_secret_creates_safe_preflight_artifact(self):
        class ReflectedHealth(BaseHTTPRequestHandler):
            posts = 0

            def do_GET(self):
                data = json.dumps({"status": "ok", "debug": "Bearer secret-benchmark-token"}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_POST(self):
                type(self).posts += 1
                self.send_response(500)
                self.end_headers()

            def log_message(self, *_args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), ReflectedHealth)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as tmp:
                output = pathlib.Path(tmp) / "benchmark.json"
                env = {
                    "SCOS_BENCHMARK_ENDPOINT": f"http://127.0.0.1:{server.server_port}",
                    "SCOS_BENCHMARK_TOKEN": self.config.token,
                    "SCOS_BENCHMARK_CONTROL_PLANE_TOKEN": self.config.control_plane_token,
                    "SCOS_BENCHMARK_TENANT_ID": self.config.tenant_id,
                    "SCOS_BENCHMARK_PRINCIPAL_ID": self.config.principal_id,
                }
                with patch.dict(os.environ, env, clear=True), patch.object(sys, "argv", ["run_deployed", "--output", str(output)]):
                    with redirect_stdout(io.StringIO()) as stdout:
                        rc = run_deployed_main()
                self.assertEqual(rc, 1)
                self.assertEqual(ReflectedHealth.posts, 0)
                artifact = json.loads(output.read_text())
                self.assertEqual(artifact["stage"], "preflight")
                self.assertEqual(artifact["cases"], [])
                for secret in (self.config.token, self.config.control_plane_token):
                    self.assertNotIn(secret, output.read_text() + stdout.getvalue())
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_non_sensitive_success_response_is_accepted(self):
        body = {"status": "ok", "version": "v1"}
        with patch.object(self.client.opener, "open", return_value=Response(json.dumps(body).encode())):
            self.assertEqual(self.client.health(), body)


if __name__ == "__main__":
    unittest.main()
