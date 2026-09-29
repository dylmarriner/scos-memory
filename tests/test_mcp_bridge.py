"""The stdio MCP bridge must relay JSON-RPC to mcp-gateway with credentials
taken from the process environment (MCP client "env" blocks), and the
per-agent copies must stay identical to the canonical bridge."""
import json
import os
import subprocess
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BRIDGE = ROOT / "integrations/agent-plugins/scos-mcp-bridge.py"
COPIES = [
    ROOT / "integrations/codex/plugins/scos-memory-fabric/scripts/scos-mcp-bridge.py",
    ROOT / "integrations/opencode/dot-opencode/scripts/scos-mcp-bridge.py",
]


class Gateway(BaseHTTPRequestHandler):
    seen = []

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        Gateway.seen.append(self.headers)
        body = json.dumps({"jsonrpc": "2.0", "id": req.get("id"), "result": {"tools": [{"name": "scos_recall"}]}}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class BridgeTests(unittest.TestCase):
    def test_relays_with_env_credentials(self):
        Gateway.seen = []
        server = HTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            env = dict(os.environ, HOME="/nonexistent", SCOS_AGENT_NAME="cline",
                       SCOS_GATEWAY_URL=f"http://127.0.0.1:{server.server_port}/mcp",
                       SCOS_BEARER_TOKEN="tok", SCOS_TENANT_ID="t-1", SCOS_PRINCIPAL_ID="p-1")
            out = subprocess.run([sys.executable, str(BRIDGE)], env=env, timeout=30, check=True,
                                 input=b'{"jsonrpc":"2.0","id":1,"method":"tools/list"}\n'
                                       b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n',
                                 capture_output=True).stdout
        finally:
            server.shutdown()
            server.server_close()
        lines = out.decode().splitlines()
        self.assertEqual(len(lines), 1, "notifications must not produce output")
        self.assertEqual(json.loads(lines[0])["result"]["tools"][0]["name"], "scos_recall")
        headers = Gateway.seen[0]
        self.assertEqual(headers["Authorization"], "Bearer tok")
        self.assertEqual(headers["X-SCOS-Tenant-ID"], "t-1")
        self.assertEqual(headers["X-SCOS-Agent-Name"], "cline")

    def test_copies_match_canonical(self):
        for copy in COPIES:
            self.assertEqual(copy.read_bytes(), BRIDGE.read_bytes(), f"{copy} drifted from {BRIDGE}")


if __name__ == "__main__":
    unittest.main()
