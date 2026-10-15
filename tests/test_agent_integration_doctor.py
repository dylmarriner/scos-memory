"""The integration doctor reports VERIFIED only from checks run on this machine."""

import importlib.util
import json
import os
import pathlib
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("agent_integration_doctor", ROOT / "scripts/agent_integration_doctor.py")
doctor = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = doctor
spec.loader.exec_module(doctor)

PLUGIN = doctor.SCOS_PLUGIN_KEY


class _Gateway:
    def __init__(self, reply: dict, status: int = 200):
        self.seen = []
        gateway = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers.get("Content-Length", "0"))
                gateway.seen.append(({k.lower(): v for k, v in self.headers.items()}, json.loads(self.rfile.read(length))))
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(reply).encode())

            def log_message(self, *args):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}/mcp"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


TOOLS = {"jsonrpc": "2.0", "id": 1, "result": {"tools": [{"name": "scos_recall"}]}}


class DoctorTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.home = pathlib.Path(tmp.name) / "home"
        self.bin = pathlib.Path(tmp.name) / "bin"
        self.home.mkdir()
        self.bin.mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith("SCOS_")}
        env["PATH"] = str(self.bin)
        patcher = mock.patch.dict(os.environ, env, clear=True)
        patcher.start()
        self.addCleanup(patcher.stop)

    def gateway(self, reply=TOOLS, status=200):
        gw = _Gateway(reply, status)
        self.addCleanup(gw.close)
        return gw

    def install_binary(self, name):
        path = self.bin / name
        path.write_text("#!/bin/sh\nexit 0\n")
        path.chmod(0o755)

    def write(self, relative, content):
        path = self.home / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    def row(self, agent_id):
        report = doctor.build_report(home=self.home)
        return next(r for r in report["agents"] if r["agent_id"] == agent_id)

    def configure_claude_plugin(self, url):
        self.write(".claude/settings.json", json.dumps({
            "enabledPlugins": {PLUGIN: True},
            "pluginConfigs": {PLUGIN: {"options": {
                "gateway_url": url, "bearer_token": "tok", "tenant_id": "t1", "principal_id": "p1",
            }}},
        }))

    def test_nothing_installed_is_missing_not_verified(self):
        self.assertEqual(self.row("claude-code")["status"], "MISSING")
        self.assertEqual(self.row("codex")["status"], "MISSING")
        self.assertEqual(doctor.build_report(home=self.home)["summary"]["verified"], 0)

    def test_config_dir_without_binary_is_not_verified(self):
        (self.home / ".claude").mkdir()
        row = self.row("claude-code")
        self.assertEqual(row["status"], "PRESENT")
        self.assertIn("binary", row["note"])

    def test_binary_without_integration_is_not_verified(self):
        self.install_binary("claude")
        row = self.row("claude-code")
        self.assertEqual(row["status"], "PRESENT")
        self.assertEqual([c["name"] for c in row["checks"]], ["binary", "integration_config"])
        self.assertFalse(row["checks"][-1]["passed"])

    def test_unreachable_gateway_is_not_verified(self):
        self.install_binary("claude")
        gw = _Gateway(TOOLS)
        url = gw.url
        gw.close()
        self.configure_claude_plugin(url)
        row = self.row("claude-code")
        self.assertEqual(row["status"], "PRESENT")
        self.assertEqual(row["checks"][-1]["name"], "mcp_gateway")
        self.assertFalse(row["checks"][-1]["passed"])

    def test_gateway_error_is_not_verified(self):
        self.install_binary("claude")
        gw = self.gateway({"error": "unauthorized"}, status=401)
        self.configure_claude_plugin(gw.url)
        self.assertEqual(self.row("claude-code")["status"], "PRESENT")

    def test_claude_code_verified_when_every_check_passes(self):
        self.install_binary("claude")
        gw = self.gateway()
        self.configure_claude_plugin(gw.url)
        row = self.row("claude-code")
        self.assertEqual(row["status"], "VERIFIED", row["note"])
        self.assertTrue(all(c["passed"] for c in row["checks"]))
        headers, body = gw.seen[0]
        self.assertEqual(body["method"], "tools/list")
        self.assertEqual(headers["authorization"], "Bearer tok")
        self.assertEqual(headers["x-scos-tenant-id"], "t1")

    def test_codex_verified_through_bridge_config(self):
        self.install_binary("codex")
        gw = self.gateway()
        self.write(".codex/config.toml", f'[plugins."{PLUGIN}"]\nenabled = true\n')
        self.write(".hermes/scos.env", f"SCOS_GATEWAY_URL={gw.url}\nSCOS_BEARER_TOKEN=abc\n"
                                       "SCOS_TENANT_ID=t\nSCOS_PRINCIPAL_ID=p\n")
        row = self.row("codex")
        self.assertEqual(row["status"], "VERIFIED", row["note"])
        self.assertEqual(gw.seen[0][0]["authorization"], "Bearer abc")

    def test_codex_disabled_plugin_is_not_verified(self):
        self.install_binary("codex")
        gw = self.gateway()
        self.write(".codex/config.toml", f'[plugins."{PLUGIN}"]\nenabled = false\n')
        self.write(".hermes/scos.env", f"SCOS_GATEWAY_URL={gw.url}\n")
        self.assertEqual(self.row("codex")["status"], "PRESENT")
        self.assertEqual(gw.seen, [])

    def test_gateway_without_tools_is_not_verified(self):
        self.install_binary("codex")
        gw = self.gateway({"jsonrpc": "2.0", "id": 1, "result": {"tools": []}})
        self.write(".codex/config.toml", "[mcp_servers.scos]\ncommand = \"python3\"\n")
        self.write(".hermes/scos.env", f"SCOS_GATEWAY_URL={gw.url}\n")
        self.assertEqual(self.row("codex")["status"], "PRESENT")


if __name__ == "__main__":
    unittest.main()
