"""Tests for benchmarks/publish.py: payload shape and fail-closed publishing."""
from __future__ import annotations

import json
import os
import socket
import sys
import threading
import traceback
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest.mock import MagicMock, patch
from urllib.error import URLError

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from benchmarks.publish import publish, run_from_cases  # noqa: E402


class _Collector(BaseHTTPRequestHandler):
    received: list[dict] = []
    status = 201
    redirect_to = None
    echo_error_body = False
    reason_phrase = None
    response_body = None

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        type(self).received.append({"path": self.path, "auth": self.headers.get("Authorization"), "body": body})
        self.send_response(type(self).status, type(self).reason_phrase)
        if type(self).redirect_to:
            self.send_header("Location", type(self).redirect_to)
            self.end_headers()
            return
        if type(self).echo_error_body:
            self.end_headers()
            self.wfile.write(("upstream echoed " + self.headers.get("Authorization", "")).encode())
            return
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(type(self).response_body if type(self).response_body is not None else
                         json.dumps({"run_id": "abc", "created": True}).encode())

    def log_message(self, *args):  # silence test output
        pass


class PublishTests(unittest.TestCase):
    def setUp(self):
        _Collector.received = []
        _Collector.status = 201
        _Collector.redirect_to = None
        _Collector.echo_error_body = False
        _Collector.reason_phrase = None
        _Collector.response_body = None
        self.server = HTTPServer(("127.0.0.1", 0), _Collector)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def test_run_from_cases_carries_measurements_only(self):
        run = run_from_cases("suite-a", [
            {"case_id": "c1", "passed": True, "latency_ms": 12.5, "details": {"x": 1}},
            {"case_id": "c2", "task_success": False, "latency_ms": 3, "metrics": {"recall": 0.5, "label": "n/a", "flag": True}},
        ], mode="m", wall_time_ms=20, recorded_at="2026-10-01T00:00:00Z")
        self.assertEqual(run["suite"], "suite-a")
        self.assertEqual(run["cases"][0], {"case_id": "c1", "latency_ms": 12.5, "passed": True})
        self.assertEqual(run["cases"][1], {"case_id": "c2", "latency_ms": 3.0, "scores": {"recall": 0.5}, "passed": False})
        self.assertNotIn("total_cases", run)

    def test_run_from_cases_rejects_nonfinite_or_negative_measurements(self):
        invalid_cases = [
            ("nan latency", {"latency_ms": float("nan")}),
            ("infinite latency", {"latency_ms": float("inf")}),
            ("negative infinite latency", {"latency_ms": -float("inf")}),
            ("negative latency", {"latency_ms": -0.5}),
            ("oversized integer latency", {"latency_ms": 10 ** 1000}),
            ("nan score", {"scores": {"recall": float("nan")}}),
            ("infinite score", {"scores": {"recall": float("inf")}}),
            ("oversized integer score", {"scores": {"recall": 10 ** 1000}}),
        ]
        for label, fields in invalid_cases:
            with self.subTest(label=label), self.assertRaises(ValueError):
                run_from_cases("suite", [{"case_id": "c1", "latency_ms": 1.0, **fields}])
        for invalid in (float("nan"), float("inf"), -1.0, 10 ** 1000, "12.5"):
            with self.subTest(wall_time="oversized" if isinstance(invalid, int) else repr(invalid)), self.assertRaises(ValueError):
                run_from_cases("suite", [{"case_id": "c1", "latency_ms": 1.0}], wall_time_ms=invalid)

    def test_run_from_cases_rejects_nonobject_scores_and_preserves_valid_values(self):
        for invalid in ([1.0], [], 0, False, ""):
            with self.subTest(scores=repr(invalid)), self.assertRaisesRegex(ValueError, "scores must be an object"):
                run_from_cases("suite", [{"case_id": "c1", "latency_ms": 0, "scores": invalid}])
        with self.assertRaisesRegex(ValueError, "scores must be an object"):
            run_from_cases("suite", [{"case_id": "c1", "latency_ms": 0, "metrics": []}])
        self.assertEqual(run_from_cases("suite", [{"case_id": "c1", "latency_ms": 0, "scores": {"recall": 1}, "metrics": []}])["cases"][0]["scores"], {"recall": 1.0})
        self.assertEqual(run_from_cases("suite", [{"case_id": "c1", "latency_ms": 0, "scores": None, "metrics": {"recall": 0.5}}])["cases"][0]["scores"], {"recall": 0.5})
        run = run_from_cases("suite", [{"case_id": "c1", "latency_ms": 0,
                                         "scores": {"signed_margin": -2.5, "recall": 0.75}}],
                             wall_time_ms=0)
        self.assertEqual(run["cases"][0]["scores"], {"signed_margin": -2.5, "recall": 0.75})
        self.assertEqual(run["cases"][0]["latency_ms"], 0.0)
        self.assertEqual(run["wall_time_ms"], 0.0)

    def test_run_from_cases_requires_actual_latency_measurements_before_http(self):
        # A missing latency previously became 0 ms and could be published as
        # measured evidence. Strings were also silently coerced to numbers.
        for label, fields in (
            ("missing", {}),
            ("null", {"latency_ms": None}),
            ("numeric string", {"latency_ms": "12.5"}),
            ("boolean", {"latency_ms": False}),
        ):
            with self.subTest(label=label):
                with self.assertRaisesRegex(ValueError, "latency"):
                    publish(run_from_cases("suite", [{"case_id": "c1", **fields}]),
                            base_url=self.url, token="t0k")
        self.assertEqual(_Collector.received, [], "unmeasured cases must never reach the publisher")
        valid = run_from_cases("suite", [{"case_id": "c1", "latency_ms": 0}])
        self.assertEqual(valid["cases"][0]["latency_ms"], 0.0)

    def test_publish_rejects_nonfinite_nested_json_before_network(self):
        for invalid in (float("nan"), float("inf"), -float("inf")):
            with self.subTest(value=repr(invalid)), self.assertRaises(RuntimeError):
                publish({"suite": "suite", "cases": [{"case_id": "c1", "latency_ms": 1,
                                                           "scores": {"recall": invalid}}]},
                        base_url=self.url, token="t0k")
        self.assertEqual(_Collector.received, [], "invalid evidence must not reach observability")

    def test_publish_rejects_ambiguous_base_urls_before_http(self):
        # Query/fragment suffixes silently change the POST path while a
        # permissive HTTP 201 responder can still claim that evidence persisted.
        invalid = (
            self.url + "?unexpected=1",
            self.url + "#unexpected",
            self.url + "?",
            self.url + "#",
            self.url.replace("http://", "http://user:password@"),
            "http://127.0.0.1:invalid-port",
        )
        for base_url in invalid:
            with self.subTest(base_url=base_url):
                with self.assertRaisesRegex(RuntimeError, "publish URL"):
                    publish({"suite": "x", "cases": []}, base_url=base_url, token="t0k")
        self.assertEqual(_Collector.received, [], "invalid destinations must not receive evidence")
        result = publish({"suite": "x", "cases": []}, base_url=self.url + "/prefix", token="t0k")
        self.assertEqual(result["run_id"], "abc")
        self.assertEqual(_Collector.received[0]["path"], "/prefix/v1/benchmarks/runs")

    def test_publish_rejects_invalid_bearer_header_before_http(self):
        for token in ("line\nfeed", "carriage\rreturn", "nonascii-\u2603"):
            with self.subTest(token=repr(token)):
                with self.assertRaisesRegex(RuntimeError, "publish token"):
                    publish({"suite": "x", "cases": []}, base_url=self.url, token=token)
        self.assertEqual(_Collector.received, [], "malformed tokens must not reach the HTTP client")

    def test_publish_rejects_remote_plaintext_before_sending_service_token(self):
        # A configured remote HTTP collector would otherwise receive the
        # internal service token in cleartext. DNS names are not loopback proof.
        invalid = (
            "http://192.0.2.17:8080", "http://10.1.2.3:8080",
            "http://localhost:8080", "http://127.0.0.1.attacker.test:8080",
            "http://[2001:db8::1]:8080",
        )
        with patch("urllib.request.OpenerDirector.open", side_effect=AssertionError("network attempted")) as network:
            for url in invalid:
                with self.subTest(url=url), self.assertRaisesRegex(RuntimeError, "HTTPS.*loopback"):
                    publish({"suite": "x", "cases": []}, base_url=url, token="service-secret")
            network.assert_not_called()
        self.assertEqual(_Collector.received, [])

    def test_remote_https_publisher_keeps_expected_path_and_bearer(self):
        # HTTPS must remain usable for production collectors; no real remote
        # endpoint or paid service is contacted by this transport mock.
        response = MagicMock()
        response.status = 201
        response.read.return_value = b'{"run_id":"secure-run","created":true}'
        response.__enter__.return_value = response
        with patch("urllib.request.OpenerDirector.open", return_value=response) as opened:
            result = publish({"suite": "x", "cases": []},
                             base_url="https://collector.example.test/api", token="service-secret")
        request = opened.call_args.args[0]
        self.assertEqual(result["run_id"], "secure-run")
        self.assertEqual(request.full_url, "https://collector.example.test/api/v1/benchmarks/runs")
        self.assertEqual(request.get_method(), "POST")
        self.assertEqual(request.get_header("Authorization"), "Bearer service-secret")

    def test_literal_ipv6_loopback_publisher_works_when_available(self):
        class IPv6HTTPServer(HTTPServer):
            address_family = socket.AF_INET6

        try:
            server = IPv6HTTPServer(("::1", 0), _Collector)
        except OSError as exc:
            self.skipTest(f"IPv6 loopback unavailable: {exc}")
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            result = publish({"suite": "x", "cases": []},
                             base_url=f"http://[::1]:{server.server_port}", token="service-secret")
            self.assertEqual(result["run_id"], "abc")
            self.assertEqual(len(_Collector.received), 1)
            self.assertEqual(_Collector.received[0]["auth"], "Bearer service-secret")
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_loopback_publisher_never_forwards_service_token_to_inherited_proxy(self):
        # HTTP_PROXY and an empty NO_PROXY must not divert local fixture
        # publication, even when the proxy returns a plausible HTTP 201.
        class ProxyCollector(_Collector):
            received = []
            status = 201
            redirect_to = None
            echo_error_body = False
            reason_phrase = None
            response_body = None

        proxy = HTTPServer(("127.0.0.1", 0), ProxyCollector)
        thread = threading.Thread(target=proxy.serve_forever, daemon=True)
        thread.start()
        try:
            env = {
                "HTTP_PROXY": f"http://127.0.0.1:{proxy.server_port}",
                "http_proxy": f"http://127.0.0.1:{proxy.server_port}",
                "NO_PROXY": "", "no_proxy": "",
            }
            with patch.dict(os.environ, env, clear=True):
                result = publish({"suite": "x", "cases": []}, base_url=self.url, token="service-secret")
            self.assertEqual(result["run_id"], "abc")
            self.assertEqual(len(_Collector.received), 1, "loopback collector must receive the run")
            self.assertEqual(_Collector.received[0]["auth"], "Bearer service-secret")
            self.assertEqual(ProxyCollector.received, [], "proxy must not see the token or benchmark")
        finally:
            proxy.shutdown()
            proxy.server_close()
            thread.join()

    def test_publish_is_noop_when_unconfigured(self):
        os.environ.pop("SCOS_BENCHMARK_PUBLISH_URL", None)
        self.assertIsNone(publish({"suite": "x", "cases": []}))

    def test_publish_posts_with_bearer(self):
        result = publish({"suite": "x", "cases": [{"case_id": "a", "latency_ms": 1}]}, base_url=self.url, token="t0k")
        self.assertEqual(result["run_id"], "abc")
        self.assertEqual(_Collector.received[0]["path"], "/v1/benchmarks/runs")
        self.assertEqual(_Collector.received[0]["auth"], "Bearer t0k")

    def test_http_error_cannot_echo_publisher_token_in_diagnostics(self):
        _Collector.status = 503
        _Collector.echo_error_body = True
        _Collector.reason_phrase = "publisher-secret-token"
        with self.assertRaises(RuntimeError) as raised:
            publish({"suite": "x", "cases": []}, base_url=self.url, token="publisher-secret-token")
        self.assertIn("HTTP 503", str(raised.exception))
        self.assertNotIn("publisher-secret-token", "".join(traceback.format_exception(raised.exception)))

    def test_transport_error_never_exposes_publisher_token_in_traceback(self):
        # A proxy or custom transport can include request headers in its
        # exception reason. This is untrusted data, just like an HTTP body.
        secret = "publisher-secret-token"
        with patch("urllib.request.OpenerDirector.open", side_effect=URLError(secret)):
            with self.assertRaisesRegex(RuntimeError, "benchmark publish failed") as raised:
                publish({"suite": "x", "cases": []}, base_url=self.url, token=secret)
        self.assertNotIn(secret, str(raised.exception))
        self.assertNotIn(secret, "".join(traceback.format_exception(raised.exception)))

    def test_redirect_never_forwards_publisher_authorization(self):
        class RedirectTarget(_Collector):
            received = []
            status = 201
            redirect_to = None
            echo_error_body = False

            def do_GET(self):  # urllib changes POST to GET on HTTP 302
                type(self).received.append({"path": self.path, "auth": self.headers.get("Authorization")})
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"run_id":"redirected"}')

        target = HTTPServer(("127.0.0.1", 0), RedirectTarget)
        thread = threading.Thread(target=target.serve_forever, daemon=True)
        thread.start()
        try:
            _Collector.status = 302
            _Collector.redirect_to = f"http://127.0.0.1:{target.server_port}/collect"
            with self.assertRaises(RuntimeError):
                publish({"suite": "x", "cases": []}, base_url=self.url, token="publisher-secret-token")
            self.assertEqual(RedirectTarget.received, [],
                             "publisher credentials must not follow an upstream redirect")
        finally:
            target.shutdown()
            target.server_close()
            thread.join()

    def test_publish_rejects_malformed_success_acknowledgements(self):
        invalid = [
            ("empty body", b""),
            ("non-json body", b"not-json"),
            ("array", b'[{"run_id":"abc","created":true}]'),
            ("missing run id", b'{"created":true}'),
            ("null run id", b'{"run_id":null,"created":true}'),
            ("empty run id", b'{"run_id":"","created":true}'),
            ("numeric run id", b'{"run_id":42,"created":true}'),
            ("whitespace run id", b'{"run_id":"   ","created":true}'),
            ("oversized run id", b'{"run_id":"' + b"x" * 129 + b'","created":true}'),
            ("nonfinite field", b'{"run_id":"abc","created":true,"extra":NaN}'),
            ("missing created", b'{"run_id":"abc"}'),
            ("nonboolean created", b'{"run_id":"abc","created":"true"}'),
            ("numeric created", b'{"run_id":"abc","created":1}'),
            ("oversized body", b'{"run_id":"abc","created":true,"padding":"' + b"x" * (1024 * 1024) + b'"}'),
        ]
        for label, response_body in invalid:
            with self.subTest(label=label):
                _Collector.response_body = response_body
                with self.assertRaisesRegex(RuntimeError, "invalid benchmark publish acknowledgement"):
                    publish({"suite": "x", "cases": []}, base_url=self.url, token="publisher-secret-token")
        self.assertEqual(len(_Collector.received), len(invalid))

    def test_publish_rejects_exponent_overflow_in_acknowledgement(self):
        # Python json.loads silently turns 1e999 into inf, even when
        # parse_constant rejects the literal Infinity/NaN spellings.
        invalid = [
            ("top-level overflow", b'{"run_id":"abc","created":true,"ratio":1e999}'),
            ("nested negative overflow", b'{"run_id":"abc","created":true,"summary":{"latency":-1e999}}'),
            ("array overflow", b'{"run_id":"abc","created":true,"summary":[0,1e999]}'),
        ]
        for label, response_body in invalid:
            with self.subTest(label=label):
                _Collector.response_body = response_body
                with self.assertRaisesRegex(RuntimeError, "invalid benchmark publish acknowledgement"):
                    publish({"suite": "x", "cases": []}, base_url=self.url, token="t0k")
        _Collector.response_body = b'{"run_id":"abc","created":true,"summary":{"ratio":1e308}}'
        self.assertEqual(publish({"suite": "x", "cases": []}, base_url=self.url, token="t0k")["summary"]["ratio"], 1e308)

    def test_publish_rejects_ambiguous_duplicate_acknowledgement_keys(self):
        # Real HTTP: a duplicate key must never be accepted as proof of persistence.
        invalid = [
            b'{"run_id":"forged","run_id":"actual","created":true}',
            b'{"run_id":"actual","created":false,"created":true}',
            b'{"run_id":"actual","created":true,"summary":{"passed":1,"passed":0}}',
        ]
        for response_body in invalid:
            with self.subTest(response_body=response_body):
                _Collector.response_body = response_body
                with self.assertRaisesRegex(RuntimeError, "invalid benchmark publish acknowledgement"):
                    publish({"suite": "x", "cases": []}, base_url=self.url, token="t0k")

    def test_publish_rejects_status_creation_mismatch(self):
        # The observability API returns 201 for a new run and 200 for an existing run.
        for status, created in ((201, False), (200, True)):
            with self.subTest(status=status, created=created):
                _Collector.status = status
                _Collector.response_body = json.dumps({"run_id": "abc", "created": created}).encode()
                with self.assertRaisesRegex(RuntimeError, "invalid benchmark publish acknowledgement"):
                    publish({"suite": "x", "cases": []}, base_url=self.url, token="t0k")

    def test_run_from_cases_rejects_ambiguous_case_identity(self):
        for bad_id in (None, False, 123, "", "  ", [], {}):
            with self.subTest(case_id=repr(bad_id)):
                with self.assertRaisesRegex(ValueError, "case_id"):
                    run_from_cases("suite", [{"case_id": bad_id, "latency_ms": 1}])
        with self.assertRaisesRegex(ValueError, "duplicate case_id"):
            run_from_cases("suite", [
                {"case_id": "same", "latency_ms": 1},
                {"case_id": "same", "latency_ms": 2},
            ])
        valid = run_from_cases("suite", [
            {"case_id": "case-a", "latency_ms": 0},
            {"case_id": "case-b", "latency_ms": 1},
        ])
        self.assertEqual([case["case_id"] for case in valid["cases"]], ["case-a", "case-b"])

    def test_publish_accepts_idempotent_existing_run_acknowledgement(self):
        _Collector.status = 200
        _Collector.response_body = b'{"run_id":"existing-run","created":false}'
        result = publish({"suite": "x", "cases": []}, base_url=self.url, token="t0k")
        self.assertEqual(result, {"run_id": "existing-run", "created": False})

    def test_publish_malformed_success_body_never_leaks_credentials(self):
        _Collector.response_body = b"publisher-secret-token\xff"
        with self.assertRaises(RuntimeError) as raised:
            publish({"suite": "x", "cases": []}, base_url=self.url, token="publisher-secret-token")
        self.assertNotIn("publisher-secret-token", "".join(traceback.format_exception(raised.exception)))

    def test_publish_fails_closed(self):
        _Collector.status = 400
        with self.assertRaises(RuntimeError):
            publish({"suite": "x", "cases": []}, base_url=self.url, token="t0k")
        with self.assertRaises(RuntimeError):
            publish({"suite": "x", "cases": []}, base_url=self.url, token="")


if __name__ == "__main__":
    unittest.main()
