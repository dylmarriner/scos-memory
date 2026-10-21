from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import sys
import tempfile
import threading
import time
import types
import unittest
import uuid
from pathlib import Path


agent_module = types.ModuleType("agent")
memory_provider_module = types.ModuleType("agent.memory_provider")


class MemoryProvider:
    pre_compress_checkpoint_api_version = 1


memory_provider_module.MemoryProvider = MemoryProvider
agent_module.memory_provider = memory_provider_module
sys.modules.setdefault("agent", agent_module)
sys.modules.setdefault("agent.memory_provider", memory_provider_module)

PLUGIN_PATH = Path(__file__).with_name("__init__.py")
spec = importlib.util.spec_from_file_location("scos_hermes_provider", PLUGIN_PATH)
module = importlib.util.module_from_spec(spec)
assert spec and spec.loader
spec.loader.exec_module(module)
ScosMemoryProvider = module.ScosMemoryProvider


class ScosMemoryProviderV2Tests(unittest.TestCase):
    def make_provider(self):
        provider = ScosMemoryProvider()
        provider._session_id = "session-alpha"
        provider._env = {
            "SCOS_GATEWAY_URL": "https://gateway.invalid",
            "SCOS_HERMES_PROVIDER_URL": "https://provider.invalid",
            "SCOS_BEARER_TOKEN": "test-token",
            "SCOS_TENANT_ID": "tenant-1",
            "SCOS_PRINCIPAL_ID": "principal-1",
            "SCOS_NAMESPACE_ID": "namespace-1",
        }
        return provider

    def test_advertises_checkpoint_api_v2(self):
        provider = self.make_provider()
        self.assertEqual(provider.pre_compress_checkpoint_api_version, 2)

    def test_pre_compress_is_durable_and_idempotent(self):
        provider = self.make_provider()
        calls = []
        provider._provider_post = lambda path, payload, timeout=8: calls.append((path, payload)) or {"synced": True}
        messages = [
            {"role": "user", "content": "remember alpha"},
            {"role": "assistant", "content": "noted"},
        ]

        provider.on_pre_compress(messages, require_checkpoint=True)
        provider.on_pre_compress(messages, require_checkpoint=True)

        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0][0], "/v1/provider/sync_turn")
        self.assertEqual(calls[0][1]["turn_id"], calls[1][1]["turn_id"])
        self.assertTrue(calls[0][1]["turn_id"].startswith("hermes-v2-checkpoint:"))
        self.assertEqual(calls[0][1]["messages"], messages)

    def test_required_checkpoint_failure_propagates(self):
        provider = self.make_provider()

        def fail(*_args, **_kwargs):
            raise RuntimeError("durable store unreachable")

        provider._provider_post = fail
        with self.assertRaisesRegex(RuntimeError, "durable store unreachable"):
            provider.on_pre_compress([{"role": "user", "content": "critical"}], require_checkpoint=True)

    def test_required_checkpoint_cannot_succeed_when_writes_are_disabled(self):
        provider = self.make_provider()
        provider._write_enabled = False
        with self.assertRaisesRegex(RuntimeError, "non-primary"):
            provider.on_pre_compress(
                [{"role": "user", "content": "critical delegated evidence"}],
                require_checkpoint=True,
            )

    def test_best_effort_checkpoint_failure_does_not_break_compression(self):
        provider = self.make_provider()
        provider._provider_post = lambda *_args, **_kwargs: (_ for _ in ()).throw(RuntimeError("offline"))
        self.assertEqual(provider.on_pre_compress([{"role": "user", "content": "ordinary"}]), "")

    def test_memory_write_is_mirrored_with_provenance(self):
        provider = self.make_provider()
        calls = []
        provider._provider_post = lambda path, payload, timeout=8: calls.append((path, payload)) or {"mirrored": True}

        provider.on_memory_write(
            "replace",
            "user",
            "prefers concise answers",
            metadata={"session_id": "s-1", "tool_name": "memory"},
        )
        provider.shutdown()

        self.assertEqual(calls[0][0], "/v1/provider/memory_write")
        payload = calls[0][1]
        self.assertEqual(payload["scope"], "user")
        self.assertEqual(payload["value"], "prefers concise answers")
        self.assertEqual(payload["source"], "hermes_memory:replace")
        self.assertIn("user", payload["key"])

    def test_remove_keys_prior_content_and_does_not_rewrite_deleted_value(self):
        provider = self.make_provider()
        calls = []
        provider._provider_post = lambda path, payload, timeout=8: calls.append((path, payload)) or {"mirrored": True}

        provider.on_memory_write(
            "remove",
            "memory",
            "",
            metadata={"old_text": "obsolete fact", "session_id": "s-1"},
        )
        provider.shutdown()

        payload = calls[0][1]
        expected_hash = hashlib.sha256(b"obsolete fact\0").hexdigest()
        self.assertEqual(payload["key"], f"hermes:memory:{expected_hash}")
        self.assertEqual(payload["value"], "")
        self.assertEqual(payload["source"], "hermes_memory:remove")

    def test_sync_turn_never_waits_for_previous_network_write(self):
        provider = self.make_provider()
        started = threading.Event()
        release = threading.Event()
        calls = []

        def post(path, payload, timeout=8):
            calls.append(payload["turn_id"])
            if len(calls) == 1:
                started.set()
                release.wait(2)
            return {"synced": True}

        provider._provider_post = post
        provider.sync_turn("first", "answer one")
        self.assertTrue(started.wait(1))

        before = time.monotonic()
        provider.sync_turn("second", "answer two")
        elapsed = time.monotonic() - before
        self.assertLess(elapsed, 0.2)

        release.set()
        provider.shutdown()
        self.assertEqual(len(calls), 2)

    def test_sync_turn_records_real_tool_calls(self):
        provider = self.make_provider()
        posted = []
        provider._provider_post = lambda path, payload, timeout=8: posted.append(payload) or {"synced": True}
        provider._gateway_rpc = lambda method, params, timeout=8: {"result": {"content": [{"type": "text", "text": "{}"}]}}

        provider.handle_tool_call("scos_recall", {"query": "deploys"})
        transcript = [
            {"role": "user", "content": "earlier"},
            {"role": "assistant", "tool_calls": [{"id": "old", "function": {"name": "stale", "arguments": "{}"}}]},
            {"role": "user", "content": "check the build"},
            {"role": "assistant", "tool_calls": [{"id": "c1", "function": {"name": "terminal", "arguments": "{\"cmd\": \"make\"}"}}]},
            {"role": "tool", "tool_call_id": "c1", "content": "ok"},
            {"role": "assistant", "content": "build passes"},
        ]
        provider.sync_turn("check the build", "build passes", messages=transcript)
        provider.sync_turn("next", "answer")
        provider.shutdown()

        first, second = posted
        names = [(c["source"], c["name"]) for c in first["tool_calls"]]
        self.assertEqual(names, [("transcript", "terminal"), ("scos_provider", "scos_recall")])
        self.assertEqual(first["tool_calls"][0]["arguments"], {"cmd": "make"})
        self.assertEqual(first["tool_calls"][0]["result"], "ok")
        self.assertEqual(first["tool_calls"][1]["status"], "ok")
        self.assertEqual(second["tool_calls"], [], "executed calls are attributed to one turn only")

    def test_queue_prefetch_and_session_end_use_lifecycle_service(self):
        provider = self.make_provider()
        calls = []
        provider._provider_post = lambda path, payload, timeout=8: calls.append((path, payload)) or {"ok": True}

        provider.queue_prefetch("next topic", session_id="session-alpha")
        provider.on_session_end([{"role": "user", "content": "final evidence"}])
        provider.shutdown()

        paths = [path for path, _ in calls]
        self.assertIn("/v1/provider/queue_prefetch", paths)
        self.assertIn("/v1/provider/session_end", paths)
        self.assertIn("/v1/provider/sync_turn", paths)

    def test_save_config_is_profile_scoped_and_excludes_secret(self):
        provider = self.make_provider()
        values = {
            "SCOS_GATEWAY_URL": "https://gateway.example",
            "SCOS_BEARER_TOKEN": "must-not-be-written",
            "SCOS_TENANT_ID": "tenant-1",
            "SCOS_PRINCIPAL_ID": "principal-1",
            "SCOS_HERMES_PROVIDER_URL": "https://provider.example",
            "SCOS_NAMESPACE_ID": "namespace-1",
        }
        with tempfile.TemporaryDirectory() as tmp:
            provider.save_config(values, tmp)
            path = Path(tmp) / "scos.json"
            stored = json.loads(path.read_text())
            self.assertNotIn("SCOS_BEARER_TOKEN", stored)
            self.assertEqual(stored["SCOS_GATEWAY_URL"], "https://gateway.example")
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)

    def test_local_mode_sends_no_credentials_and_acts_as_the_local_identity(self):
        local = "00000000-0000-0000-0000-000000000001"
        with tempfile.TemporaryDirectory() as tmp:
            # A config left over from another deployment names its own
            # identity; local mode replaces it with the reserved one.
            (Path(tmp) / "scos.json").write_text(json.dumps({
                "SCOS_TENANT_ID": "tenant-elsewhere",
                "SCOS_PRINCIPAL_ID": "principal-elsewhere",
                "SCOS_NAMESPACE_ID": "namespace-elsewhere",
            }))
            (Path(tmp) / "scos.env").write_text(
                "SCOS_AUTH_MODE=local\n"
                "SCOS_GATEWAY_URL=https://gateway.invalid/mcp\n"
                "SCOS_HERMES_PROVIDER_URL=https://provider.invalid\n"
            )
            provider = ScosMemoryProvider()
            provider._hermes_home = Path(tmp)
            saved = {k: os.environ.pop(k) for k in list(os.environ) if k.startswith("SCOS_")}
            try:
                provider._env = provider._load_env()
            finally:
                os.environ.update(saved)
        self.assertTrue(provider.is_available())
        self.assertTrue(provider._persistence_configured())
        self.assertEqual(provider._service_headers(), {"Content-Type": "application/json"})
        payload = provider._session_payload("session-alpha")
        self.assertEqual((payload["tenant_id"], payload["principal_id"], payload["namespace_id"]), (local, local, local))

    def test_without_local_mode_credentials_are_still_required(self):
        provider = ScosMemoryProvider()
        provider._env = {
            "SCOS_GATEWAY_URL": "https://gateway.invalid",
            "SCOS_HERMES_PROVIDER_URL": "https://provider.invalid",
        }
        self.assertFalse(provider.is_available())
        self.assertFalse(provider._persistence_configured())
        with self.assertRaises(RuntimeError):
            provider._service_headers()

    def test_local_mode_setup_needs_only_the_service_urls(self):
        provider = ScosMemoryProvider()
        with tempfile.TemporaryDirectory() as tmp:
            provider.save_config({
                "SCOS_AUTH_MODE": "local",
                "SCOS_GATEWAY_URL": "https://gateway.example/mcp",
                "SCOS_HERMES_PROVIDER_URL": "https://provider.example",
            }, tmp)
            stored = json.loads((Path(tmp) / "scos.json").read_text())
        self.assertEqual(stored["SCOS_AUTH_MODE"], "local")

    def test_tool_discovery_falls_back_to_minimum_safe_catalogue(self):
        provider = self.make_provider()
        provider._gateway_rpc = lambda *_args, **_kwargs: {"error": {"message": "offline"}}
        names = {tool["name"] for tool in provider.get_tool_schemas()}
        self.assertEqual(names, {"scos_recall", "scos_status"})

    def test_stable_session_ids_are_deterministic_and_preserve_uuid_input(self):
        generated_a = module._stable_session_uuid("human-session", "tenant-1", "principal-1")
        generated_b = module._stable_session_uuid("human-session", "tenant-1", "principal-1")
        self.assertEqual(generated_a, generated_b)
        uuid.UUID(generated_a)

        supplied = "123e4567-e89b-12d3-a456-426614174000"
        self.assertEqual(module._stable_session_uuid(supplied, "x", "y"), supplied)


if __name__ == "__main__":
    unittest.main()
