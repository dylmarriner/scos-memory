import io
import json
import os
import pathlib
import sys
import tempfile
import threading
import time
import unittest
from contextlib import redirect_stdout
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch
from urllib.error import HTTPError

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from benchmarks.deployed import BenchmarkConfig, DeployedCodeClient, config_from_environment  # noqa: E402
from benchmarks.run_deployed import DeployedPreflightError, NOT_RUN_EXIT, fixture_files, fixture_identity, run  # noqa: E402
from benchmarks.run_deployed import main as run_deployed_main  # noqa: E402


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

    def test_config_rejects_nonfinite_or_nonpositive_timeouts(self):
        for timeout in (float("nan"), float("inf"), float("-inf"), 0.0, -1.0):
            with self.subTest(timeout=repr(timeout)):
                with self.assertRaisesRegex(RuntimeError, "timeout.*positive.*finite"):
                    BenchmarkConfig(
                        endpoint="https://example.test", token="secret-token",
                        tenant_id="tenant-1", principal_id="principal-1",
                        timeout_seconds=timeout,
                    )

    def test_config_rejects_oversized_integer_timeout_with_controlled_error(self):
        # math.isfinite(10 ** 1000) raises OverflowError before the normal
        # configuration error; callers must see the same safe rejection.
        with self.assertRaisesRegex(RuntimeError, "timeout.*positive.*finite"):
            BenchmarkConfig(
                endpoint="https://example.test", token="secret-token",
                tenant_id="tenant-1", principal_id="principal-1",
                timeout_seconds=10 ** 1000,
            )

    def test_environment_rejects_nonfinite_timeouts(self):
        for timeout in ("NaN", "Infinity", "-Infinity"):
            with self.subTest(timeout=timeout):
                with patch.dict(os.environ, {
                    "SCOS_BENCHMARK_ENDPOINT": "https://example.test",
                    "SCOS_BENCHMARK_TOKEN": "secret-token",
                    "SCOS_BENCHMARK_TENANT_ID": "tenant-1",
                    "SCOS_BENCHMARK_PRINCIPAL_ID": "principal-1",
                    "SCOS_BENCHMARK_TIMEOUT_SECONDS": timeout,
                }, clear=True):
                    with self.assertRaisesRegex(RuntimeError, "timeout.*positive.*finite"):
                        config_from_environment()

    def test_config_rejects_endpoint_userinfo(self):
        with self.assertRaisesRegex(RuntimeError, "userinfo"):
            BenchmarkConfig(
                endpoint="https://user:password@linode.example.test:8084",
                token="secret-token", tenant_id="tenant-1", principal_id="principal-1",
            )


    def test_config_rejects_ambiguous_or_malformed_endpoint_authority(self):
        for endpoint in (
            "https:///no-host", "https://example.test/path?token=untrusted",
            "https://example.test/path#fragment", "https://example.test:invalid",
            "https://example.test:65536", "https://[::1",
        ):
            with self.subTest(endpoint=endpoint):
                with self.assertRaisesRegex(RuntimeError, "SCOS_BENCHMARK_ENDPOINT"):
                    BenchmarkConfig(
                        endpoint=endpoint, token="secret-token",
                        tenant_id="tenant-1", principal_id="principal-1",
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

    def test_http_error_diagnostics_never_read_untrusted_response_body(self):
        class TrackedBody(io.BytesIO):
            def __init__(self, body):
                super().__init__(body)
                self.read_sizes = []

            def read(self, size=-1):
                self.read_sizes.append(size)
                return super().read(size)

        body = TrackedBody(b"x" * 50000)
        error = HTTPError(
            "http://127.0.0.1/error", 502, "Bad Gateway", {}, body
        )
        client = DeployedCodeClient(BenchmarkConfig(
            endpoint=f"http://127.0.0.1:{self.server.server_port}",
            token="secret-token", tenant_id="tenant-1", principal_id="principal-1",
        ))
        with patch.object(client.opener, "open", side_effect=error):
            with self.assertRaisesRegex(RuntimeError, "HTTP 502"):
                client.health()
        self.assertEqual(body.read_sizes, [], "untrusted error bodies must not be read")
        self.assertTrue(body.closed)

    def test_redirect_never_forwards_benchmark_credentials(self):
        # A benchmark endpoint must not be able to redirect the bearer and
        # tenant/principal headers to another service.
        capture_start = len(_Handler.requests)

        class RedirectHandler(BaseHTTPRequestHandler):
            status = 302

            def do_GET(self):
                self.send_response(self.status)
                self.send_header(
                    "Location", f"http://127.0.0.1:{DeployedCodeClientTests.server.server_port}/capture"
                )
                self.end_headers()

            do_POST = do_GET

            def log_message(self, *_args):
                return

        redirect = ThreadingHTTPServer(("127.0.0.1", 0), RedirectHandler)
        worker = threading.Thread(target=redirect.serve_forever, daemon=True)
        worker.start()
        try:
            client = DeployedCodeClient(BenchmarkConfig(
                endpoint=f"http://127.0.0.1:{redirect.server_port}",
                token="must-not-leak", tenant_id="tenant-1", principal_id="principal-1",
            ))
            for status in (301, 302, 303, 307, 308):
                RedirectHandler.status = status
                for operation in (client.health, client.ensure_repository):
                    with self.subTest(status=status, operation=operation.__name__):
                        with self.assertRaisesRegex(RuntimeError, f"HTTP {status}"):
                            operation()
                        self.assertEqual(_Handler.requests[capture_start:], [])
        finally:
            redirect.shutdown()
            worker.join()
            redirect.server_close()

    def test_success_body_is_bounded_even_when_json_is_valid(self):
        # Real HTTP: a 200 response with valid JSON plus excessive trailing
        # whitespace used to be accepted after an unbounded response.read().
        class OversizedHandler(BaseHTTPRequestHandler):
            def do_GET(self):
                body = b'{"status":"ok"}' + b" " * (16 * 1024 * 1024 + 1)
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                try:
                    self.wfile.write(body)
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def log_message(self, *_args):
                return

        server = ThreadingHTTPServer(("127.0.0.1", 0), OversizedHandler)
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            client = DeployedCodeClient(BenchmarkConfig(
                endpoint=f"http://127.0.0.1:{server.server_port}",
                token="secret-token", tenant_id="tenant-1", principal_id="principal-1",
            ))
            with self.assertRaisesRegex(RuntimeError, "response exceeds"):
                client.health()
        finally:
            server.shutdown()
            worker.join()
            server.server_close()

    def test_real_http_rejects_ambiguous_and_nonfinite_json_evidence(self):
        class UntrustedJSONHandler(BaseHTTPRequestHandler):
            raw_body = b""

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(self.raw_body)))
                self.end_headers()
                self.wfile.write(self.raw_body)

            def log_message(self, *_args):
                return

        server = ThreadingHTTPServer(("127.0.0.1", 0), UntrustedJSONHandler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            client = DeployedCodeClient(BenchmarkConfig(
                endpoint=f"http://127.0.0.1:{server.server_port}",
                token="secret-token", tenant_id="tenant-1", principal_id="principal-1",
            ))
            for body in (
                b'{"status":"not_ready","status":"ready"}',
                b'{"status":"not_ready","\\u0073tatus":"ready"}',
                b'{"status":"ready","metadata":{"trusted":true,"trusted":false}}',
                b'{"status":"ready","diagnostics":NaN}',
                b'{"status":"ready","diagnostics":Infinity}',
                b'{"status":"ready","diagnostics":-Infinity}',
            ):
                with self.subTest(body=body):
                    UntrustedJSONHandler.raw_body = body
                    with self.assertRaisesRegex(RuntimeError, "invalid JSON"):
                        client.health()
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

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


class _CodeService(BaseHTTPRequestHandler):
    """A code service with per-branch snapshots: exact path matches, lexical
    fallback over content, rename diffs and idempotent re-indexing. With
    ``leak`` set, a query on any branch also searches every other branch."""
    leak = False
    preflight_failure = False
    health_status = "ok"
    raw_health = None
    case_query_error = False
    delay: dict = {}
    branches: dict = {}
    snapshots: dict = {}

    def _send(self, payload):
        data = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):  # noqa: N802
        if self.raw_health is not None and self.path == "/health/ready":
            data = self.raw_health
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        if self.preflight_failure and self.path == "/health/ready":
            data = b"upstream failed: secret-benchmark-token"
            self.send_response(503)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        status = self.health_status if self.path == "/health/ready" else "ok"
        self._send({"status": status, "version": "v", "source_sha": "sha", "build_time": "now"})

    def do_POST(self):  # noqa: N802
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
        time.sleep(self.delay.get(self.path, 0))
        if self.case_query_error and self.path == "/v2/code/query":
            # A compromised upstream can echo request credentials in error bodies.
            data = b"upstream failed: secret-benchmark-token secret-control-plane-token"
            self.send_response(503)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        if self.path == "/v2/code/repos":
            return self._send({"repository_id": "repo-1"})
        if self.path == "/v2/code/index":
            snapshot_id = f"{body['branch']}@{body['commit_id']}"
            if snapshot_id not in self.snapshots:
                files = {f["path"]: f for f in body["files"]}
                self.snapshots[snapshot_id] = {"files": files, "parent": self.branches.get(body["branch"])}
            self.branches[body["branch"]] = snapshot_id
            return self._send({"snapshot_id": snapshot_id})
        if self.path == "/v2/code/snapshots/diff":
            snapshot = self.snapshots[body["snapshot_id"]]
            previous = self.snapshots[snapshot["parent"]]["files"] if snapshot["parent"] else {}
            renamed = [{"Path": path, "PreviousPath": old}
                       for path, f in snapshot["files"].items() if path not in previous
                       for old, g in previous.items()
                       if old not in snapshot["files"] and g["content_hash"] == f["content_hash"]]
            return self._send({"Renamed": renamed})
        if self.path == "/v2/code/query":
            searched = list(self.branches.values()) if self.leak else [self.branches[body["branch"]]]
            files = {}
            for snapshot_id in searched:
                files.update(self.snapshots[snapshot_id]["files"])
            if body["query"] in files:
                return self._send({"results": [{"Path": body["query"], "MatchType": "exact"}], "retrieval_mode": "exact"})
            word = body["query"].split(".")[0].lower()
            hits = [{"Path": p, "MatchType": "lexical"} for p, f in files.items() if word in f["content"].lower()]
            return self._send({"results": hits, "retrieval_mode": "lexical"})
        self.send_response(404)
        self.end_headers()

    def log_message(self, *_args):
        return


class DeployedRunTests(unittest.TestCase):
    def setUp(self):
        _CodeService.leak = False
        _CodeService.preflight_failure = False
        _CodeService.health_status = "ok"
        _CodeService.raw_health = None
        _CodeService.case_query_error = False
        _CodeService.delay = {}
        _CodeService.branches = {}
        _CodeService.snapshots = {}
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _CodeService)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.config = BenchmarkConfig(endpoint=f"http://127.0.0.1:{self.server.server_port}", token="t",
                                      tenant_id="tenant-1", principal_id="principal-1")

    def test_cases_pass_against_a_correct_service_and_are_timed_individually(self):
        _CodeService.delay = {"/v2/code/snapshots/diff": 0.2}
        artifact = run(self.config, run_nonce="n1")
        self.assertEqual({c["case_id"]: c["passed"] for c in artifact["cases"]},
                         {"exact_symbol_lookup": True, "rename_lineage": True, "branch_isolation": True,
                          "retry_idempotency": True})
        latencies = {c["case_id"]: c["latency_ms"] for c in artifact["cases"]}
        # Only rename_lineage asks for a diff, so only it carries the delay.
        self.assertGreaterEqual(latencies["rename_lineage"], 200)
        self.assertLess(latencies["exact_symbol_lookup"], 200)
        self.assertLess(latencies["branch_isolation"], 200)

    def test_branch_isolation_fails_when_files_cross_branches(self):
        _CodeService.leak = True
        cases = {c["case_id"]: c for c in run(self.config, run_nonce="n2")["cases"]}
        self.assertFalse(cases["branch_isolation"]["passed"])
        self.assertEqual(cases["branch_isolation"]["details"]["branch_file_on_main"], ["branch.go"])

    def test_failed_live_preflight_writes_failure_artifact_without_secrets_or_publication(self):
        _CodeService.preflight_failure = True
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "failed.json"
            argv = ["run_deployed", "--output", str(output)]
            env = {
                "SCOS_BENCHMARK_ENDPOINT": self.config.endpoint,
                "SCOS_BENCHMARK_TOKEN": "secret-benchmark-token",
                "SCOS_BENCHMARK_TENANT_ID": self.config.tenant_id,
                "SCOS_BENCHMARK_PRINCIPAL_ID": self.config.principal_id,
                "SCOS_BENCHMARK_PUBLISH_URL": "http://127.0.0.1:1",
            }
            with patch.dict(os.environ, env, clear=True), patch.object(sys, "argv", argv), \
                    patch("benchmarks.run_deployed.publish") as publish_mock:
                output_stream = io.StringIO()
                with redirect_stdout(output_stream):
                    exit_code = run_deployed_main()
                publish_mock.assert_not_called()
            self.assertEqual(exit_code, 1)
            self.assertTrue(output.exists(), "configured preflight failures must leave durable evidence")
            raw = output.read_text()
            artifact = json.loads(raw)
            self.assertEqual(artifact["status"], "failed")
            self.assertEqual(artifact["cases"], [])
            self.assertEqual(artifact["stage"], "preflight")
            self.assertNotIn("secret-benchmark-token", raw + output_stream.getvalue())
            self.assertNotIn("passed_cases", artifact)

    def test_http_200_unready_status_fails_preflight_before_seeding(self):
        # HTTP 200 is not evidence of readiness when the body says otherwise.
        for status in ("not_ready", "unready", "error", None, False, 0):
            with self.subTest(status=status):
                _CodeService.health_status = status
                with self.assertRaises(DeployedPreflightError):
                    run(self.config, run_nonce="not-ready")
                self.assertEqual(_CodeService.snapshots, {})
                self.assertEqual(_CodeService.branches, {})

    def test_ambiguous_ready_json_fails_preflight_without_seeding_or_publication(self):
        _CodeService.raw_health = b'{"status":"not_ready","status":"ready"}'
        with self.assertRaises(DeployedPreflightError):
            run(self.config, run_nonce="duplicate-ready")
        self.assertEqual(_CodeService.snapshots, {})
        self.assertEqual(_CodeService.branches, {})

        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "duplicate-ready.json"
            env = {
                "SCOS_BENCHMARK_ENDPOINT": self.config.endpoint,
                "SCOS_BENCHMARK_TOKEN": "secret-benchmark-token",
                "SCOS_BENCHMARK_TENANT_ID": self.config.tenant_id,
                "SCOS_BENCHMARK_PRINCIPAL_ID": self.config.principal_id,
            }
            with patch.dict(os.environ, env, clear=True), \
                    patch.object(sys, "argv", ["run_deployed", "--output", str(output)]), \
                    patch("benchmarks.run_deployed.publish") as publish_mock:
                stream = io.StringIO()
                with redirect_stdout(stream):
                    exit_code = run_deployed_main()
                publish_mock.assert_not_called()
            self.assertEqual(exit_code, 1)
            artifact = json.loads(output.read_text())
            self.assertEqual(artifact["status"], "failed")
            self.assertEqual(artifact["stage"], "preflight")
            self.assertEqual(artifact["cases"], [])
            self.assertNotIn("secret-benchmark-token", output.read_text() + stream.getvalue())
            self.assertEqual(_CodeService.snapshots, {})
            self.assertEqual(_CodeService.branches, {})

    def test_http_200_ready_status_is_accepted(self):
        _CodeService.health_status = "ready"
        artifact = run(self.config, run_nonce="ready")
        self.assertEqual(artifact["summary"]["passed_cases"], artifact["summary"]["total_cases"])

    def test_http_200_unready_status_writes_failed_artifact_without_publication(self):
        _CodeService.health_status = "not_ready"
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "unready.json"
            argv = ["run_deployed", "--output", str(output)]
            env = {
                "SCOS_BENCHMARK_ENDPOINT": self.config.endpoint,
                "SCOS_BENCHMARK_TOKEN": "secret-benchmark-token",
                "SCOS_BENCHMARK_TENANT_ID": self.config.tenant_id,
                "SCOS_BENCHMARK_PRINCIPAL_ID": self.config.principal_id,
            }
            with patch.dict(os.environ, env, clear=True), patch.object(sys, "argv", argv), \
                    patch("benchmarks.run_deployed.publish") as publish_mock:
                publish_mock.return_value = None
                stream = io.StringIO()
                with redirect_stdout(stream):
                    exit_code = run_deployed_main()
                publish_mock.assert_not_called()
            self.assertEqual(exit_code, 1)
            artifact = json.loads(output.read_text())
            self.assertEqual(artifact["status"], "failed")
            self.assertEqual(artifact["stage"], "preflight")
            self.assertEqual(artifact["cases"], [])
            self.assertNotIn("secret-benchmark-token", output.read_text() + stream.getvalue())
            self.assertEqual(_CodeService.snapshots, {})

    def test_case_http_error_cannot_leak_credentials_to_artifact_or_publisher(self):
        # Unlike preflight, per-case failures used to serialize upstream 503
        # response bodies directly into durable and published evidence.
        _CodeService.case_query_error = True
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "failed-case.json"
            argv = ["run_deployed", "--output", str(output)]
            env = {
                "SCOS_BENCHMARK_ENDPOINT": self.config.endpoint,
                "SCOS_BENCHMARK_TOKEN": "secret-benchmark-token",
                "SCOS_BENCHMARK_CONTROL_PLANE_TOKEN": "secret-control-plane-token",
                "SCOS_BENCHMARK_TENANT_ID": self.config.tenant_id,
                "SCOS_BENCHMARK_PRINCIPAL_ID": self.config.principal_id,
            }
            with patch.dict(os.environ, env, clear=True), patch.object(sys, "argv", argv), \
                    patch("benchmarks.run_deployed.publish") as publish_mock:
                publish_mock.return_value = {"run_id": "test"}
                stream = io.StringIO()
                with redirect_stdout(stream):
                    exit_code = run_deployed_main()
            self.assertEqual(exit_code, 1)
            artifact = json.loads(output.read_text())
            self.assertEqual(artifact["status"], "measured")
            self.assertLess(artifact["summary"]["passed_cases"], artifact["summary"]["total_cases"])
            self.assertTrue(any(not c["passed"] for c in artifact["cases"]))
            published = str(publish_mock.call_args)
            for secret in ("secret-benchmark-token", "secret-control-plane-token"):
                self.assertNotIn(secret, output.read_text() + stream.getvalue() + published)
            self.assertIn("HTTP 503", output.read_text())

    def test_unconfigured_deployment_is_recorded_as_not_run(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "out.json"
            argv = ["run_deployed", "--output", str(output)]
            with patch.dict(os.environ, {}, clear=True), patch.object(sys, "argv", argv):
                with redirect_stdout(io.StringIO()):
                    self.assertEqual(run_deployed_main(), NOT_RUN_EXIT)
            artifact = json.loads(output.read_text())
        self.assertEqual(artifact["status"], "not_run")
        self.assertIn("SCOS_BENCHMARK_ENDPOINT", artifact["reason"])
        self.assertEqual(artifact["cases"], [])


if __name__ == "__main__":
    unittest.main()
