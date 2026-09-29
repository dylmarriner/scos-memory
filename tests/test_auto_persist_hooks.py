"""End-to-end check of the Claude Code and Codex Stop hooks: each must send a
memory_write and a UUID-session sync_turn to hermes-provider, idempotently."""
import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
import uuid
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CLAUDE_HOOK = ROOT / "integrations/claude/plugins/scos-memory-fabric/hooks/scos-auto-persist.py"
CODEX_HOOK = ROOT / "integrations/codex/plugins/scos-memory-fabric/scripts/scos-auto-persist.py"


class Recorder(BaseHTTPRequestHandler):
    requests = []

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        Recorder.requests.append((self.path, self.headers, body))
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *args):
        pass


class AutoPersistHookTests(unittest.TestCase):
    def setUp(self):
        Recorder.requests = []
        self.server = HTTPServer(("127.0.0.1", 0), Recorder)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.home = tempfile.TemporaryDirectory()
        hermes = Path(self.home.name, ".hermes")
        hermes.mkdir()
        (hermes / "scos.env").write_text(
            f"SCOS_HERMES_PROVIDER_URL=http://127.0.0.1:{self.server.server_port}\n"
            "SCOS_HERMES_BEARER_TOKEN=tok\nSCOS_NAMESPACE_ID=ns-1\n"
            "SCOS_TENANT_ID=t-1\nSCOS_PRINCIPAL_ID=p-1\n"
        )

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.home.cleanup()

    def run_hook(self, hook, payload, *args):
        env = dict(os.environ, HOME=self.home.name)
        subprocess.run([sys.executable, str(hook), *args], input=json.dumps(payload).encode(),
                       env=env, check=True, timeout=30)

    def assert_persisted(self, source, key_prefix):
        paths = [p for p, _, _ in Recorder.requests]
        self.assertEqual(paths, ["/v1/provider/memory_write", "/v1/provider/sync_turn"])
        _, headers, write = Recorder.requests[0]
        self.assertEqual(headers["Authorization"], "Bearer tok")
        self.assertEqual(headers["X-SCOS-Tenant-ID"], "t-1")
        self.assertEqual(write["source"], source)
        self.assertTrue(write["key"].startswith(key_prefix + ":sess-1:"))
        _, _, sync = Recorder.requests[1]
        uuid.UUID(sync["session_id"])  # provider rejects non-UUID sessions
        self.assertEqual([m["role"] for m in sync["messages"]], ["user", "assistant"])
        self.assertEqual(sync["namespace_id"], "ns-1")
        return write["key"], sync["turn_id"]

    def test_claude_hook_persists_idempotently(self):
        transcript = Path(self.home.name, "t.jsonl")
        transcript.write_text("\n".join(json.dumps(e) for e in [
            {"message": {"role": "user", "content": "what port does the gateway use?"}},
            {"message": {"role": "assistant", "content": [{"type": "text", "text": "8080"}]}},
        ]))
        payload = {"session_id": "sess-1", "transcript_path": str(transcript)}
        self.run_hook(CLAUDE_HOOK, payload)
        first = self.assert_persisted("claude-code-stop-hook", "claude-code-turn")
        Recorder.requests = []
        self.run_hook(CLAUDE_HOOK, payload)
        self.assertEqual(self.assert_persisted("claude-code-stop-hook", "claude-code-turn"), first)

    def test_codex_hook_persists(self):
        transcript = Path(self.home.name, "rollout.jsonl")
        transcript.write_text(json.dumps({"payload": {"type": "message", "role": "user",
                              "content": [{"type": "input_text", "text": "rename the service"}]}}))
        self.run_hook(CODEX_HOOK, {"session_id": "sess-1", "transcript_path": str(transcript),
                                   "last_assistant_message": "done"})
        self.assert_persisted("codex-stop-hook", "codex-turn")

    def test_claude_session_end_closes_the_same_provider_session(self):
        transcript = Path(self.home.name, "t.jsonl")
        transcript.write_text("\n".join(json.dumps(e) for e in [
            {"message": {"role": "user", "content": "hi"}},
            {"message": {"role": "assistant", "content": [{"type": "text", "text": "hello"}]}},
        ]))
        self.run_hook(CLAUDE_HOOK, {"session_id": "sess-1", "transcript_path": str(transcript)})
        synced = Recorder.requests[1][2]["session_id"]
        Recorder.requests = []
        self.run_hook(CLAUDE_HOOK, {"session_id": "sess-1"}, "session-end")
        self.assertEqual([p for p, _, _ in Recorder.requests], ["/v1/provider/session_end"])
        self.assertEqual(Recorder.requests[0][2], {"session_id": synced})

    def test_plugins_register_their_hook_scripts(self):
        for manifest, scripts in [
            (ROOT / "integrations/claude/plugins/scos-memory-fabric/hooks/hooks.json",
             {"UserPromptSubmit": "scos-auto-recall.py", "Stop": "scos-auto-persist.py",
              "PostToolUse": "scos-tool-guidance.py", "SessionEnd": "scos-auto-persist.py session-end"}),
            (ROOT / "integrations/codex/plugins/scos-memory-fabric/hooks.json",
             {"UserPromptSubmit": "scos-auto-recall.py", "Stop": "scos-auto-persist.py",
              "PostToolUse": "scos-tool-guidance.py"}),
        ]:
            hooks = json.loads(manifest.read_text())["hooks"]
            for event, script in scripts.items():
                commands = [h["command"] for entry in hooks.get(event, []) for h in entry["hooks"]]
                self.assertTrue(any(c.endswith(script) for c in commands), f"{manifest.name} {event}: {commands}")


if __name__ == "__main__":
    unittest.main()
