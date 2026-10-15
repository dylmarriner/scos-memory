"""Tests for benchmarks/publish.py: payload shape and fail-closed publishing."""
from __future__ import annotations

import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from benchmarks.publish import publish, run_from_cases  # noqa: E402


class _Collector(BaseHTTPRequestHandler):
    received: list[dict] = []
    status = 201

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        type(self).received.append({"path": self.path, "auth": self.headers.get("Authorization"), "body": body})
        self.send_response(type(self).status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"run_id": "abc", "created": True}).encode())

    def log_message(self, *args):  # silence test output
        pass


class PublishTests(unittest.TestCase):
    def setUp(self):
        _Collector.received = []
        _Collector.status = 201
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

    def test_publish_is_noop_when_unconfigured(self):
        os.environ.pop("SCOS_BENCHMARK_PUBLISH_URL", None)
        self.assertIsNone(publish({"suite": "x", "cases": []}))

    def test_publish_posts_with_bearer(self):
        result = publish({"suite": "x", "cases": [{"case_id": "a", "latency_ms": 1}]}, base_url=self.url, token="t0k")
        self.assertEqual(result["run_id"], "abc")
        self.assertEqual(_Collector.received[0]["path"], "/v1/benchmarks/runs")
        self.assertEqual(_Collector.received[0]["auth"], "Bearer t0k")

    def test_publish_fails_closed(self):
        _Collector.status = 400
        with self.assertRaises(RuntimeError):
            publish({"suite": "x", "cases": []}, base_url=self.url, token="t0k")
        with self.assertRaises(RuntimeError):
            publish({"suite": "x", "cases": []}, base_url=self.url, token="")


if __name__ == "__main__":
    unittest.main()
