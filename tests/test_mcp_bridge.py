"""The stdio MCP bridge relays single messages and JSON-RPC batches to the
gateway and answers malformed input with JSON-RPC errors instead of
crashing or going silent."""
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = pathlib.Path(__file__).parents[1]
BRIDGES = [
    ROOT / "integrations/agent-plugins/scos-mcp-bridge.py",
    ROOT / "integrations/codex/plugins/scos-memory-fabric/scripts/scos-mcp-bridge.py",
    ROOT / "integrations/opencode/dot-opencode/scripts/scos-mcp-bridge.py",
]


class FakeGateway(BaseHTTPRequestHandler):
    def log_message(self, *args):
        return

    def do_POST(self):  # noqa: N802
        msg = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if "id" not in msg:
            self.send_response(202)
            self.end_headers()
            return
        body = json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": {"echo": msg["method"]}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class BridgeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), FakeGateway)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def run_bridge(self, bridge, lines):
        with tempfile.TemporaryDirectory() as home:
            env_dir = pathlib.Path(home) / ".hermes"
            env_dir.mkdir()
            (env_dir / "scos.env").write_text(
                f"SCOS_GATEWAY_URL=http://127.0.0.1:{self.server.server_port}/mcp\n"
                "SCOS_BEARER_TOKEN=t\nSCOS_TENANT_ID=tenant\nSCOS_PRINCIPAL_ID=principal\n")
            done = subprocess.run([sys.executable, str(bridge)], input="\n".join(lines) + "\n", text=True,
                                  capture_output=True, timeout=30, env=dict(os.environ, HOME=home))
        self.assertEqual(done.returncode, 0, done.stderr)
        return [json.loads(line) for line in done.stdout.splitlines()]

    LINES = [
        json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/list"}),
        json.dumps([{"jsonrpc": "2.0", "id": 2, "method": "a"},
                    {"jsonrpc": "2.0", "method": "notifications/initialized"},
                    {"jsonrpc": "2.0", "id": 3, "method": "b"}]),
        json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}),
        "{not json",
        "[]",
        json.dumps([{"jsonrpc": "2.0", "method": "notifications/cancelled"}]),
        json.dumps({"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": [1]}),
    ]

    def check(self, out):
        self.assertEqual(out[0], {"jsonrpc": "2.0", "id": 1, "result": {"echo": "tools/list"}})
        self.assertEqual([r["id"] for r in out[1]], [2, 3])
        self.assertEqual(out[2]["error"]["code"], -32700)
        self.assertEqual(out[3]["error"]["code"], -32600)
        # Array params are relayed, not a crash.
        self.assertEqual(out[4]["id"], 4)
        self.assertEqual(len(out), 5, out)

    @unittest.skipUnless(shutil.which("node"), "node is not installed")
    def test_desktop_extension_relay(self):
        # The extension relays asynchronously, so lines are fed one at a time
        # and each answer awaited, keeping the output in input order.
        relay = ROOT / "integrations/claude/desktop-extension/server/index.js"
        env = dict(os.environ, SCOS_GATEWAY_URL=f"http://127.0.0.1:{self.server.server_port}/mcp",
                   SCOS_BEARER_TOKEN="t", SCOS_TENANT_ID="tenant", SCOS_PRINCIPAL_ID="principal")
        proc = subprocess.Popen(["node", str(relay)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True, env=env)
        out = []
        try:
            for line, answered in zip(self.LINES, [True, True, False, True, True, False, True]):
                proc.stdin.write(line + "\n")
                proc.stdin.flush()
                if answered:
                    out.append(json.loads(proc.stdout.readline()))
        finally:
            proc.stdin.close()
            rest = proc.stdout.read()
            proc.wait(timeout=10)
        self.assertEqual(rest.strip(), "", "unexpected output for a notification")
        self.check(out)

    def test_single_batch_notification_and_malformed_input(self):
        for bridge in BRIDGES:
            with self.subTest(bridge=bridge.relative_to(ROOT)):
                self.check(self.run_bridge(bridge, self.LINES))


if __name__ == "__main__":
    unittest.main()
