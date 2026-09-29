import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

from scos_agent.client import AgentAdapterClient, EnvelopeNotFoundError, GatewayError


class Gateway(BaseHTTPRequestHandler):
    def _send(self, code, body):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _authorized(self):
        if self.headers.get("Authorization") != "Bearer jwt":
            self._send(401, {"error": "unauthorized"})
            return False
        return True

    def do_GET(self):
        if not self._authorized():
            return
        url = urlparse(self.path)
        if url.path == "/v1/envelope" and parse_qs(url.query).get("session_id") == ["sess 1"]:
            self._send(200, {"agent_id": "agent-1", "session_id": "sess 1", "facts": ["prefers Go"],
                             "format": "native", "unknown_future_field": True})
        else:
            self._send(404, {"error": "envelope not found"})

    def do_POST(self):
        if not self._authorized():
            return
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self._send(200, {"format": body["target_format"], "content": f"# envelope for {body['session_id']}"})

    def log_message(self, *args):
        pass


class AgentClientTests(unittest.TestCase):
    def setUp(self):
        self.server = HTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def test_get_and_render_envelope(self):
        client = AgentAdapterClient("agent-1", self.url, bearer_token="jwt")
        env = client.get_envelope("sess 1")
        self.assertEqual((env.agent_id, env.session_id, env.facts), ("agent-1", "sess 1", ["prefers Go"]))
        rendered = client.render_envelope("sess 1", "markdown")
        self.assertEqual(rendered.content, "# envelope for sess 1")
        with self.assertRaises(EnvelopeNotFoundError):
            client.get_envelope("missing")

    def test_failures_raise_instead_of_fabricating(self):
        with self.assertRaises(GatewayError):
            AgentAdapterClient("agent-1", self.url, bearer_token="wrong").get_envelope("sess 1")
        # A port that was bound and released has nothing listening on it.
        closed = HTTPServer(("127.0.0.1", 0), Gateway)
        closed_url = f"http://127.0.0.1:{closed.server_port}"
        closed.server_close()
        with self.assertRaises(GatewayError):
            AgentAdapterClient("agent-1", closed_url, bearer_token="jwt", timeout=1).get_envelope("sess 1")

    def test_requires_identity_and_url(self):
        with self.assertRaises(ValueError):
            AgentAdapterClient("agent-1", self.url)
        with self.assertRaises(ValueError):
            AgentAdapterClient("agent-1", "", bearer_token="jwt")


if __name__ == "__main__":
    unittest.main()
