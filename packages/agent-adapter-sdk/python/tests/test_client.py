import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

from scos_agent.client import AgentAdapterClient, EnvelopeError, EnvelopeNotFound


class _Gateway:
    """A local HTTP server answering GET /v1/envelope with a fixed reply."""

    def __init__(self, status: int, body: bytes):
        gateway = self
        self.requests = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                gateway.requests.append((self.path, self.headers.get("Authorization")))
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


def _envelope(**overrides):
    body = {
        "tenant_id": "tenant-1",
        "agent_id": "agent-1",
        "session_id": "sess 1",
        "facts": ["fact-a"],
        "code_references": ["a.go:10"],
        "task_state": "reviewing",
        "format": "json",
    }
    body.update(overrides)
    return json.dumps(body).encode("utf-8")


class AgentClientTests(unittest.TestCase):
    def client(self, status, body):
        gw = _Gateway(status, body)
        self.addCleanup(gw.close)
        return gw, AgentAdapterClient("agent-1", gw.url + "/", "tok-1")

    def test_fetch_envelope_returns_stored_envelope(self):
        gw, cli = self.client(200, _envelope())
        env = cli.fetch_envelope("sess 1")
        self.assertEqual(env.facts, ["fact-a"])
        self.assertEqual(env.code_references, ["a.go:10"])
        self.assertEqual(env.task_state, "reviewing")
        self.assertEqual(env.learnings, [])
        self.assertEqual(gw.requests[0], ("/v1/envelope?session_id=sess%201", "Bearer tok-1"))

    def test_not_found(self):
        _, cli = self.client(404, b"envelope not found")
        with self.assertRaises(EnvelopeNotFound) as ctx:
            cli.fetch_envelope("sess 1")
        self.assertEqual(ctx.exception.status, 404)

    def test_refusal_is_surfaced_with_status(self):
        _, cli = self.client(401, b"authenticated token has no tenant and principal claims")
        with self.assertRaises(EnvelopeError) as ctx:
            cli.fetch_envelope("sess 1")
        self.assertEqual(ctx.exception.status, 401)
        self.assertIn("no tenant", str(ctx.exception))

    def test_unreachable_gateway_raises(self):
        gw = _Gateway(200, b"{}")
        url = gw.url
        gw.close()
        with self.assertRaises(EnvelopeError):
            AgentAdapterClient("agent-1", url, "tok", timeout=1).fetch_envelope("s")

    def test_envelope_for_other_agent_or_session_is_rejected(self):
        for body in (_envelope(agent_id="agent-2"), _envelope(session_id="other"), b"not json", b"[]"):
            _, cli = self.client(200, body)
            with self.assertRaises(EnvelopeError):
                cli.fetch_envelope("sess 1")

    def test_malformed_fields_are_rejected(self):
        _, cli = self.client(200, _envelope(facts="all"))
        with self.assertRaises(EnvelopeError):
            cli.fetch_envelope("sess 1")

    def test_required_config(self):
        for args in (("", "http://x", "t"), ("a", "", "t"), ("a", "http://x", "")):
            with self.assertRaises(ValueError):
                AgentAdapterClient(*args)


if __name__ == "__main__":
    unittest.main()
