"""Hermes scos-memory plugin: tools come from the gateway, recall sessions
are scoped per identity, and a missing/unreachable gateway degrades to the
core tools instead of crashing registration."""
import importlib.util
import json
import os
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest import mock

PLUGIN = Path(__file__).resolve().parent.parent / "integrations/hermes/plugins/scos-memory/__init__.py"


def load_plugin():
    spec = importlib.util.spec_from_file_location("scos_memory_plugin", PLUGIN)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class Gateway(BaseHTTPRequestHandler):
    calls = []

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        Gateway.calls.append(req)
        if req["method"] == "tools/list":
            result = {"tools": [{"name": "scos_recall", "inputSchema": {"type": "object"}},
                                {"name": "scos_code_query", "inputSchema": {"type": "object"}}]}
        else:
            result = {"content": [{"type": "text", "text": json.dumps({"ok": True})}]}
        body = json.dumps({"jsonrpc": "2.0", "id": req["id"], "result": result}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class Ctx:
    def __init__(self):
        self.tools = {}

    def register_tool(self, name, handler, **kwargs):
        self.tools[name] = handler


class ScosMemoryPluginTests(unittest.TestCase):
    def setUp(self):
        Gateway.calls = []
        self.server = HTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def env(self, principal):
        return {"HOME": "/nonexistent", "SCOS_GATEWAY_URL": f"http://127.0.0.1:{self.server.server_port}/mcp",
                "SCOS_BEARER_TOKEN": "tok", "SCOS_TENANT_ID": "t-1", "SCOS_PRINCIPAL_ID": principal}

    def recall_session(self, principal, **kwargs):
        with mock.patch.dict(os.environ, self.env(principal), clear=True):
            plugin = load_plugin()
            ctx = Ctx()
            plugin.register(ctx)
            self.assertEqual(set(ctx.tools), {"scos_recall", "scos_code_query"})
            self.assertEqual(json.loads(ctx.tools["scos_recall"]({"query": "q"}, **kwargs)), {"ok": True})
        return Gateway.calls[-1]["params"]["arguments"]["session_id"]

    def test_recall_session_is_scoped_per_identity(self):
        alice, bob = self.recall_session("alice"), self.recall_session("bob")
        self.assertNotEqual(alice, bob)
        self.assertEqual(alice, self.recall_session("alice"))
        self.assertNotEqual(self.recall_session("alice", session_id="hermes-42"), alice)

    def test_unconfigured_gateway_degrades_without_crashing(self):
        with mock.patch.dict(os.environ, {"HOME": "/nonexistent"}, clear=True):
            plugin = load_plugin()
            ctx = Ctx()
            with self.assertLogs(level="WARNING"):
                plugin.register(ctx)
            self.assertEqual(set(ctx.tools), {"scos_recall", "scos_status"})
            self.assertIn("error", json.loads(ctx.tools["scos_status"]({})))


if __name__ == "__main__":
    unittest.main()
