"""Tests for the §32 worker metric emitter and the consolidation rates."""

import json
import os
import sys
import threading
import unittest
from collections import Counter
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation import telemetry, worker

REPO_WORKERS = Path(__file__).resolve().parents[2]


class _Collector(BaseHTTPRequestHandler):
    received: list = []
    status = 202

    def do_POST(self):  # noqa: N802 (http.server API)
        body = self.rfile.read(int(self.headers["Content-Length"]))
        type(self).received.append((self.path, dict(self.headers), json.loads(body)))
        self.send_response(type(self).status)
        self.end_headers()

    def log_message(self, *_):
        pass


class EmitterTests(unittest.TestCase):
    def setUp(self):
        _Collector.received = []
        _Collector.status = 202
        self.server = HTTPServer(("127.0.0.1", 0), _Collector)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.env = patch.dict(os.environ, {
            "SCOS_OBSERVABILITY_URL": f"http://127.0.0.1:{self.server.server_port}/",
            "SCOS_INTERNAL_SERVICE_TOKEN": "svc-token",
        })
        self.env.start()

    def tearDown(self):
        self.env.stop()
        self.server.shutdown()
        self.server.server_close()

    def test_posts_packages_telemetry_wire_format(self):
        ok = telemetry.emit("consolidation", [
            telemetry.Point(telemetry.PROMOTION_RATE, 0.5, "ratio", {"cycle": "1"}, "tenant-a"),
        ])
        self.assertTrue(ok)
        path, headers, body = _Collector.received[0]
        self.assertEqual(path, "/v1/metrics/ingest")
        self.assertEqual(headers["Authorization"], "Bearer svc-token")
        self.assertEqual(len(body), 1)
        point = body[0]
        # Exactly the observability MetricPoint field names: its decoder
        # rejects unknown fields.
        self.assertLessEqual(set(point), {"name", "value", "unit", "timestamp", "labels", "tenant"})
        self.assertEqual(point["name"], "scos.consolidation.promotion_rate")
        self.assertEqual(point["value"], 0.5)
        self.assertEqual(point["unit"], "ratio")
        self.assertEqual(point["tenant"], "tenant-a")
        self.assertEqual(point["labels"], {"cycle": "1", "service": "consolidation"})
        self.assertTrue(point["timestamp"].endswith("Z"))

    def test_rejected_delivery_returns_false_without_raising(self):
        _Collector.status = 503
        self.assertFalse(telemetry.emit("consolidation", [telemetry.Point("m", 1)]))

    def test_unreachable_collector_returns_false_without_raising(self):
        with patch.dict(os.environ, {"SCOS_OBSERVABILITY_URL": "http://127.0.0.1:9"}):
            self.assertFalse(telemetry.emit("consolidation", [telemetry.Point("m", 1)]))

    def test_disabled_without_endpoint(self):
        with patch.dict(os.environ, {"SCOS_OBSERVABILITY_URL": ""}):
            self.assertFalse(telemetry.enabled())
            self.assertFalse(telemetry.emit("consolidation", [telemetry.Point("m", 1)]))
        self.assertEqual(_Collector.received, [])


class ConsolidationRateTests(unittest.TestCase):
    def test_rates_are_fractions_of_committed_candidates(self):
        with patch.object(worker.telemetry, "emit") as emit:
            worker._emit_consolidation_rates(Counter(candidates=4, promoted=1, duplicates=2, conflicted=1))
        service, points = emit.call_args.args
        self.assertEqual(service, "consolidation")
        self.assertEqual({p.name: p.value for p in points}, {
            telemetry.PROMOTION_RATE: 0.25,
            telemetry.DUPLICATE_RATE: 0.5,
            telemetry.CONFLICT_RATE: 0.25,
        })

    def test_no_candidates_emits_nothing(self):
        with patch.object(worker.telemetry, "emit") as emit:
            worker._emit_consolidation_rates(Counter())
        emit.assert_not_called()


class CopiesTests(unittest.TestCase):
    def test_worker_emitter_copies_are_identical(self):
        # Each worker image ships its own copy; they must not drift.
        canonical = (REPO_WORKERS / "consolidation/src/scos_consolidation/telemetry.py").read_bytes()
        for copy in ("index-maintenance/telemetry.py", "policy-evaluation/telemetry.py"):
            self.assertEqual((REPO_WORKERS / copy).read_bytes(), canonical, copy)


if __name__ == "__main__":
    unittest.main()
