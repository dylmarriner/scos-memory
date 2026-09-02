import json
import os
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

from benchmarks.deployed import BenchmarkConfig, DeployedCodeClient, config_from_environment
from benchmarks.run_deployed import fixture_files, fixture_identity


class _Handler(BaseHTTPRequestHandler):
    requests = []

    def do_GET(self):
        self.requests.append((self.command, self.path, dict(self.headers), None))
        payload = {"status": "ok", "version": "test-version", "source_sha": "sha-test", "build_time": "now"}
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        self.requests.append((self.command, self.path, dict(self.headers), json.loads(body)))
        if self.path.startswith("/invalid/"):
            data = b"not-json"
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        response = {"repository_id": "repo-1"} if self.path.endswith("/repos") else {"results": []}
        data = json.dumps(response).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_args):
        return


class DeployedBenchmarkConfigTests(unittest.TestCase):
    def test_config_requires_all_remote_identity_fields(self):
        with patch.dict(os.environ, {}, clear=True):
            with self.assertRaisesRegex(RuntimeError, "SCOS_BENCHMARK_ENDPOINT"):
                config_from_environment()

    def test_config_does_not_include_credentials_in_public_metadata(self):
        config = BenchmarkConfig(
            endpoint="https://linode.example.test:8084/",
            token="secret-token",
            tenant_id="tenant-1",
            principal_id="principal-1",
            repository_path="benchmark/repo",
        )
        self.assertNotIn(config.token, config.metadata())
        self.assertEqual(config.metadata()["endpoint"], "https://linode.example.test:8084")

    def test_config_rejects_endpoint_userinfo(self):
        with self.assertRaisesRegex(RuntimeError, "userinfo"):
            BenchmarkConfig(
                endpoint="https://user:password@linode.example.test:8084",
                token="secret-token", tenant_id="tenant-1", principal_id="principal-1",
            )


class DeployedCodeClientTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        _Handler.requests = []
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.thread.join()

    def test_requests_carry_bearer_and_scoped_identity(self):
        client = DeployedCodeClient(BenchmarkConfig(
            endpoint=f"http://127.0.0.1:{self.server.server_port}",
            token="secret-token", tenant_id="tenant-1", principal_id="principal-1",
            repository_path="benchmark/repo",
        ))
        self.assertEqual(client.health()["status"], "ok")
        client.ensure_repository()
        _, path, headers, _ = _Handler.requests[-1]
        self.assertEqual(path, "/v2/code/repos")
        self.assertEqual(headers["Authorization"], "Bearer secret-token")
        self.assertEqual(headers["X-Scos-Tenant-Id"], "tenant-1")
        self.assertEqual(headers["X-Scos-Principal-Id"], "principal-1")

    def test_version_accepts_current_release_shape(self):
        client = DeployedCodeClient(BenchmarkConfig(
            endpoint=f"http://127.0.0.1:{self.server.server_port}",
            token="secret-token", tenant_id="tenant-1", principal_id="principal-1",
        ))
        self.assertEqual(client.version()["source_sha"], "sha-test")

    def test_invalid_json_response_fails_closed(self):
        client = DeployedCodeClient(BenchmarkConfig(
            endpoint=f"http://127.0.0.1:{self.server.server_port}/invalid",
            token="secret-token", tenant_id="tenant-1", principal_id="principal-1",
            repository_path="benchmark/repo",
        ))
        with self.assertRaises(RuntimeError):
            client.query("repo-1", "main", "Missing")


class DeployedFixtureTests(unittest.TestCase):
    def test_fixture_stages_are_deterministic_and_exercise_rename(self):
        initial = fixture_files("initial")
        renamed = fixture_files("renamed")
        self.assertEqual(initial, fixture_files("initial"))
        self.assertEqual(initial[0]["content"], renamed[0]["content"])
        self.assertNotEqual(initial[0]["path"], renamed[0]["path"])
        self.assertEqual(initial[0]["content_hash"], renamed[0]["content_hash"])

    def test_fixture_identity_is_stable_for_a_benchmark_tenant(self):
        config = BenchmarkConfig(
            endpoint="https://linode.example.test:8084",
            token="secret-token", tenant_id="tenant-1", principal_id="principal-1",
            repository_path="benchmark/repo",
        )
        self.assertEqual(fixture_identity(config), fixture_identity(config))


if __name__ == "__main__":
    unittest.main()
