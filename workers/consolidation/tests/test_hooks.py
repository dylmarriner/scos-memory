"""Tests for §22.6 lifecycle hook dispatch from the consolidation worker."""

import base64
import hashlib
import hmac
import json
import os
import sys
import threading
import time
import unittest
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest.mock import MagicMock, patch
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation import hooks, worker
from scos_consolidation.models import EvidenceReference, MemoryCandidate, MemoryClass, SensitivityClass
from scos_consolidation.promoter import PromotionDecision

SECRET = "worker-hook-secret"
HEADER = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"

# auth.SignClaims output for the same fixed input, asserted on the Go side by
# services/memory-intelligence TestPythonMintedClaimsTokenMatchesSignClaims.
GO_SIGNED_FIXTURE = (
    "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhZ2VudF9pZCI6IiIsImRlbGVnYXRlZF9hdXRob3JpdHkiOiIiLCJleHAiOjQxMDI0NDQ4MDAsImp0aSI6IiIsInByaW5jaXBhbF9pZCI6InByaW5jaXBhbC0xIiwic2NvcGVzIjpudWxsLCJzdWIiOiJzZXJ2aWNlOmNvbnNvbGlkYXRpb24td29ya2VyIiwidGVuYW50X2lkIjoidGVuYW50LVx1MDAzY1x1MDAyNlx1MDAzZS3DqSIsInRyYWNlX2lkIjoiIn0"
    ".jbU3TlNrnUVGKCdEqt6yDk01geYtRw2tDIjCwtwDOO8"
)


def _unb64(part: str) -> bytes:
    return base64.urlsafe_b64decode(part + "=" * (-len(part) % 4))


def validate_claims(token: str, secret: str) -> dict:
    """Reimplementation of packages/auth validateClaims."""
    parts = token.split(".")
    if len(parts) != 3 or parts[0] != HEADER:
        raise ValueError("invalid bearer claims")
    expected = hmac.new(secret.encode(), f"{parts[0]}.{parts[1]}".encode(), hashlib.sha256).digest()
    if not hmac.compare_digest(_unb64(parts[2]), expected):
        raise ValueError("invalid bearer signature")
    claims = json.loads(_unb64(parts[1]))
    if not claims.get("tenant_id") or not claims.get("principal_id"):
        raise ValueError("tenant_id and principal_id claims are required")
    if claims.get("exp", 0) <= time.time():
        raise ValueError("expired")
    return claims


class _ExtensionManager(BaseHTTPRequestHandler):
    received: list = []

    def do_POST(self):  # noqa: N802 (http.server API)
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        auth = self.headers.get("Authorization", "")
        try:
            claims = validate_claims(auth.removeprefix("Bearer "), SECRET)
        except ValueError:
            self.send_response(401)
            self.end_headers()
            return
        type(self).received.append((self.path, claims, body))
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"results":[]}')

    def log_message(self, *_):
        pass


class _ServerCase(unittest.TestCase):
    def setUp(self):
        _ExtensionManager.received = []
        self.server = HTTPServer(("127.0.0.1", 0), _ExtensionManager)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.env = patch.dict(os.environ, {
            "SCOS_EXTENSION_MANAGER_URL": f"http://127.0.0.1:{self.server.server_port}/",
            "SCOS_SECURITY_ISSUER_SECRET": SECRET,
        })
        self.env.start()

    def tearDown(self):
        self.env.stop()
        self.server.shutdown()
        self.server.server_close()


class SignClaimsTests(unittest.TestCase):
    def test_matches_go_sign_claims_byte_for_byte(self):
        token = hooks.sign_claims("tenant-<&>-é", "principal-1", "service:consolidation-worker",
                                  "cross-language-secret", 4102444800)
        self.assertEqual(token, GO_SIGNED_FIXTURE)

    def test_validates_and_rejects_tampering(self):
        token = hooks.sign_claims("t1", "p1", "service:consolidation-worker", SECRET, int(time.time()) + 60)
        claims = validate_claims(token, SECRET)
        self.assertEqual((claims["tenant_id"], claims["principal_id"], claims["sub"]), ("t1", "p1", "service:consolidation-worker"))
        with self.assertRaises(ValueError):
            validate_claims(token, "other-secret")
        with self.assertRaises(ValueError):
            hooks.sign_claims("t1", "", "s", SECRET, int(time.time()) + 60)


class DispatchTests(_ServerCase):
    def test_dispatch_is_accepted_by_claims_only_endpoint(self):
        ok = hooks.dispatch(hooks.Event(hooks.ON_MEMORY_PROMOTED, "tenant-1", "principal-1", {"memory_ids": ["m1"]}))
        self.assertTrue(ok)
        path, claims, body = _ExtensionManager.received[0]
        self.assertEqual(path, "/v1/hooks/dispatch")
        self.assertEqual(claims["sub"], "service:consolidation-worker")
        self.assertLessEqual(claims["exp"], time.time() + 61)
        self.assertEqual(body, {"event": "on_memory_promoted", "tenant_id": "tenant-1",
                                "principal_id": "principal-1", "payload": {"memory_ids": ["m1"]}})

    def test_failures_are_logged_not_raised(self):
        with patch.dict(os.environ, {"SCOS_SECURITY_ISSUER_SECRET": ""}):
            self.assertFalse(hooks.dispatch(hooks.Event("on_memory_candidate", "t", "p", {})))
        with patch.dict(os.environ, {"SCOS_EXTENSION_MANAGER_URL": "http://127.0.0.1:1"}):
            self.assertFalse(hooks.dispatch(hooks.Event("on_memory_candidate", "t", "p", {})))
        with patch.dict(os.environ, {"SCOS_EXTENSION_MANAGER_URL": ""}):
            self.assertIsNone(hooks.notify([hooks.Event("on_memory_candidate", "t", "p", {})]))
        self.assertEqual(_ExtensionManager.received, [])


class _FakeConn:
    """Records commit order; conn.transaction() rolls back on exception."""

    def __init__(self, log):
        self.log = log

    @contextmanager
    def transaction(self):
        try:
            yield
        except Exception:
            self.log.append("rollback")
            raise

    def commit(self):
        self.log.append("commit")


class CycleOrderingTests(_ServerCase):
    def test_events_sent_only_after_commit_and_never_for_a_failed_job(self):
        log: list = []
        good, bad = {"id": "job-good"}, {"id": "job-bad"}

        def fake_process_job(conn, job, events):
            events.append(hooks.Event(hooks.ON_MEMORY_PROMOTED, "tenant-1", "principal-1", {"memory_ids": [job["id"]]}))
            if job is bad:
                raise RuntimeError("write failed after the event was collected")
            return worker.Counter()

        real_notify = hooks.notify

        def recording_notify(events):
            events = list(events)
            log.append(("notify", [e.payload["memory_ids"][0] for e in events]))
            thread = real_notify(events)
            if thread:
                thread.join(5)

        with patch.object(worker, "_claim_jobs", return_value=[good, bad]), \
                patch.object(worker, "_process_job", side_effect=fake_process_job), \
                patch.object(worker, "_mark_failed"), \
                patch.object(worker.hooks, "notify", side_effect=recording_notify):
            worker._process_cycle(_FakeConn(log))

        self.assertEqual(log, ["commit", "commit", ("notify", ["job-good"]), "rollback", "commit", ("notify", [])])
        self.assertEqual([b["payload"]["memory_ids"] for _, _, b in _ExtensionManager.received], [["job-good"]])


class ProcessJobEventTests(unittest.TestCase):
    def test_job_collects_candidate_and_promotion_events_without_content(self):
        tenant, namespace, actor = uuid4(), uuid4(), str(uuid4())
        candidate = MemoryCandidate(
            candidate_id=uuid4(), tenant_id=tenant, namespace_id=namespace, source_event_id=uuid4(),
            memory_class=MemoryClass.SEMANTIC, title="Deploy target", content="secret-ish body text",
            authority_level=70, confidence_score=0.85, sensitivity_class=SensitivityClass.INTERNAL,
            evidence=[EvidenceReference(kind="event", reference_id="e1")], metadata={"actor_id": actor},
        )
        dedup = MagicMock(candidate=candidate, action="new", merged_into=None, superseded_by=None)
        events: list = []
        with patch.multiple(
            worker,
            _load_event_for_turn=MagicMock(return_value={"session_id": "s", "aggregate_id": "t", "actor_id": actor}),
            _load_session_messages=MagicMock(return_value=[]),
            extract_candidates=MagicMock(return_value=[candidate]),
            enrich_candidates=MagicMock(side_effect=lambda c: c),
            _load_existing_memory=MagicMock(return_value=[]),
            deduplicate=MagicMock(return_value=[dedup]),
            detect_conflicts=MagicMock(return_value=[]),
            evaluate_promotion=MagicMock(return_value=PromotionDecision(candidate.candidate_id, "promote", "ok", "active")),
            requires_human_review=MagicMock(return_value=(False, "")),
            _promote_candidate=MagicMock(),
            _mark_completed=MagicMock(),
        ):
            worker._process_job(MagicMock(), {"id": "j", "tenant_id": tenant, "payload": {"turn_id": "t"}}, events)

        self.assertEqual([e.name for e in events], ["on_memory_candidate", "on_memory_promoted"])
        for e in events:
            self.assertEqual((e.tenant_id, e.principal_id), (str(tenant), actor))
            self.assertEqual(e.payload["memory_ids"], [str(candidate.candidate_id)])
            self.assertEqual(e.payload["namespace_id"], str(namespace))
            self.assertNotIn("secret-ish", json.dumps(e.payload))
            self.assertNotIn("Deploy target", json.dumps(e.payload))


if __name__ == "__main__":
    unittest.main()
