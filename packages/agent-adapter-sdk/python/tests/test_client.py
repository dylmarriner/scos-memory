import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

from scos_agent.client import AgentAdapterClient, EnvelopeRenderError


class _Gateway:
    """A local HTTP server answering /v1/envelope/render with a fixed reply."""

    def __init__(self, status: int, body: bytes):
        gateway = self
        self.requests = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers.get("Content-Length", "0"))
                gateway.requests.append((self.path, json.loads(self.rfile.read(length))))
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
        "envelope_id": "env-42",
        "session_id": "sess-1",
        "tenant_id": "tenant-1",
        "agent_id": "agent-1",
        "context_items": ["item-a"],
        "permissions": ["read"],
        "generated_at": "2026-10-15T09:30:00.123456789Z",
    }
    body.update(overrides)
    return json.dumps(body).encode("utf-8")


class AgentClientTests(unittest.TestCase):
    def gateway(self, status, body):
        gw = _Gateway(status, body)
        self.addCleanup(gw.close)
        return gw

    def test_render_envelope_returns_the_gateway_envelope(self):
        gw = self.gateway(200, _envelope())
        env = AgentAdapterClient("agent-1", gw.url).render_envelope("sess-1", "tenant-1")
        self.assertEqual(env.envelope_id, "env-42")
        self.assertEqual(env.permissions, ["read"])
        self.assertEqual(env.context_items, ["item-a"])
        self.assertEqual(env.generated_at.year, 2026)
        path, sent = gw.requests[0]
        self.assertEqual(path, "/v1/envelope/render")
        self.assertEqual(sent, {"agent_id": "agent-1", "session_id": "sess-1", "tenant_id": "tenant-1"})

    def test_unreachable_gateway_raises_and_grants_nothing(self):
        gw = _Gateway(200, b"{}")
        url = gw.url
        gw.close()
        with self.assertRaises(EnvelopeRenderError):
            AgentAdapterClient("agent-1", url, timeout=1).render_envelope("sess-1", "tenant-1")

    def test_gateway_refusal_is_surfaced_with_status(self):
        gw = self.gateway(403, b'{"error":"forbidden"}')
        with self.assertRaises(EnvelopeRenderError) as ctx:
            AgentAdapterClient("agent-1", gw.url).render_envelope("sess-1", "tenant-1")
        self.assertEqual(ctx.exception.status, 403)

    def test_non_envelope_response_is_rejected(self):
        gw = self.gateway(200, b'{"format":"markdown","content":"# ctx"}')
        with self.assertRaises(EnvelopeRenderError):
            AgentAdapterClient("agent-1", gw.url).render_envelope("sess-1", "tenant-1")

    def test_malformed_json_is_rejected(self):
        gw = self.gateway(200, b"not json")
        with self.assertRaises(EnvelopeRenderError):
            AgentAdapterClient("agent-1", gw.url).render_envelope("sess-1", "tenant-1")

    def test_envelope_for_another_identity_is_rejected(self):
        gw = self.gateway(200, _envelope(tenant_id="tenant-2"))
        with self.assertRaises(EnvelopeRenderError):
            AgentAdapterClient("agent-1", gw.url).render_envelope("sess-1", "tenant-1")

    def test_malformed_permissions_are_rejected(self):
        gw = self.gateway(200, _envelope(permissions="admin"))
        with self.assertRaises(EnvelopeRenderError):
            AgentAdapterClient("agent-1", gw.url).render_envelope("sess-1", "tenant-1")

    def test_agent_id_is_required(self):
        with self.assertRaises(ValueError):
            AgentAdapterClient("")


if __name__ == "__main__":
    unittest.main()
