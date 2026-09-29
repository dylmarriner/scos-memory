import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
HOOKS = [
    ROOT / "integrations/claude/plugins/scos-memory-fabric/hooks/scos-auto-recall.py",
    ROOT / "integrations/codex/plugins/scos-memory-fabric/scripts/scos-auto-recall.py",
]


class Gateway(BaseHTTPRequestHandler):
    status = 200
    requests = []

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        Gateway.requests.append((self.headers, body))
        self.send_response(Gateway.status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        if Gateway.status == 200:
            self.wfile.write(json.dumps({"jsonrpc": "2.0", "id": 1, "result": {
                "content": [{"type": "text", "text": "gateway listens on 8098"}]}}).encode())
        else:
            self.wfile.write(b'{"error":"denied"}')

    def log_message(self, *args):
        pass


class AutoRecallHookTests(unittest.TestCase):
    def setUp(self):
        Gateway.status, Gateway.requests = 200, []
        self.server = HTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.home = tempfile.TemporaryDirectory()
        hermes = Path(self.home.name, ".hermes")
        hermes.mkdir()
        (hermes / "scos.env").write_text(
            f"SCOS_GATEWAY_URL=http://127.0.0.1:{self.server.server_port}/mcp\nSCOS_BEARER_TOKEN=tok\n")

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.home.cleanup()

    def run_hook(self, hook):
        out = subprocess.run([sys.executable, str(hook)], input=json.dumps({"prompt": "what port?"}).encode(),
                             env=dict(os.environ, HOME=self.home.name), capture_output=True, check=True, timeout=30)
        return out.stdout.decode()

    def test_recall_injects_context(self):
        for hook in HOOKS:
            Gateway.requests = []
            ctx = json.loads(self.run_hook(hook))["hookSpecificOutput"]
            self.assertEqual(ctx["hookEventName"], "UserPromptSubmit")
            self.assertIn("gateway listens on 8098", ctx["additionalContext"])
            headers, body = Gateway.requests[0]
            self.assertEqual(headers["Authorization"], "Bearer tok")
            self.assertEqual(body["params"], {"name": "scos_recall", "arguments": {"query": "what port?"}})

    def test_failure_is_logged_and_prompt_not_blocked(self):
        Gateway.status = 403
        for hook in HOOKS:
            self.assertEqual(self.run_hook(hook), "")
        log = Path(self.home.name, ".hermes/auto-recall.log").read_text()
        self.assertEqual(log.count("scos_recall failed: HTTP 403"), len(HOOKS))


if __name__ == "__main__":
    unittest.main()
