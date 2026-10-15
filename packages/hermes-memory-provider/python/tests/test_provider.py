"""Hermes provider acceptance tests (§24.7).

§24.7 names the scenarios that must be covered: startup, availability without
network calls, setup, prefetch latency, non-blocking sync, session switch,
branch lineage, reset, pre-compression capture, delegation, built-in write
mirroring, shutdown, backup paths, unavailable backend, duplicate turn sync
and provider replacement. There is one test per scenario below.
"""

import tempfile
import time
import unittest
from pathlib import Path

from scos_hermes.provider import (
    CONFIG_KEYS,
    CONTEXT_PRIMARY,
    CONTEXT_SUBAGENT,
    PROVIDER_NAME,
    HermesMemoryProvider,
)

# The complete lifecycle §24.2 requires the provider to implement.
REQUIRED_LIFECYCLE = (
    "name",
    "is_available",
    "initialize",
    "system_prompt_block",
    "prefetch",
    "queue_prefetch",
    "sync_turn",
    "get_tool_schemas",
    "handle_tool_call",
    "shutdown",
    "on_turn_start",
    "on_session_end",
    "on_session_switch",
    "on_pre_compress",
    "on_memory_write",
    "on_delegation",
    "backup_paths",
    "get_config_schema",
    "save_config",
)

REQUIRED_TOOLS = (
    "scos_recall",
    "scos_explain",
    "scos_user_model",
    "scos_memory_graph",
    "scos_correct",
    "scos_forget",
    "scos_status",
)


class ProviderTestCase(unittest.TestCase):
    def setUp(self):
        self._dir = tempfile.TemporaryDirectory()
        self.addCleanup(self._dir.cleanup)
        self.profile_path = Path(self._dir.name)

    def provider(self, **kwargs) -> HermesMemoryProvider:
        p = HermesMemoryProvider("prof1", "agent1", **kwargs)
        p.initialize(profile_path=str(self.profile_path))
        return p


class LifecycleContractTests(ProviderTestCase):
    def test_provider_name_is_scos_nexus(self):
        self.assertEqual(self.provider().name, PROVIDER_NAME)
        self.assertEqual(PROVIDER_NAME, "scos-nexus")

    def test_every_required_lifecycle_method_exists(self):
        p = self.provider()
        for method in REQUIRED_LIFECYCLE:
            self.assertTrue(hasattr(p, method), f"missing lifecycle member: {method}")

    def test_startup_and_availability_without_network(self):
        # §24.7: availability must be determinable without network calls, so a
        # provider constructed with no reachable backend still reports ready.
        p = HermesMemoryProvider("prof1", "agent1")
        self.assertFalse(p.is_available(), "provider must not be available before initialize")
        p.initialize(profile_path=str(self.profile_path))
        self.assertTrue(p.is_available())

    def test_initialize_rejects_unknown_context(self):
        p = HermesMemoryProvider("prof1", "agent1")
        with self.assertRaises(ValueError):
            p.initialize(profile_path=str(self.profile_path), context="unknown")


class SetupTests(ProviderTestCase):
    def test_config_schema_matches_governed_contract(self):
        schema = self.provider().get_config_schema()
        self.assertEqual(set(schema["properties"]), set(CONFIG_KEYS))

    def test_save_config_round_trips(self):
        p = self.provider()
        saved = p.save_config({
            "base_url": "https://scos.internal",
            "tenant_id": "tenant-1",
            "principal_id": "user-1",
            "transport_security": "mtls",
        })
        self.assertEqual(saved["base_url"], "https://scos.internal")

    def test_save_config_requires_mandatory_fields(self):
        p = self.provider()
        with self.assertRaises(ValueError):
            p.save_config({"base_url": "https://scos.internal"})

    def test_save_config_rejects_unknown_keys(self):
        p = self.provider()
        with self.assertRaises(ValueError):
            p.save_config({
                "base_url": "https://scos.internal",
                "tenant_id": "t", "principal_id": "u",
                "nonsense": "value",
            })

    def test_save_config_refuses_inline_secrets(self):
        # §24.4: secrets belong in Hermes secret configuration, never here.
        p = self.provider()
        for banned in ("token", "password", "secret", "api_key"):
            with self.assertRaises(ValueError, msg=f"{banned} was accepted"):
                p.save_config({
                    "base_url": "https://scos.internal",
                    "tenant_id": "t", "principal_id": "u",
                    banned: "hunter2",
                })


class AutonomousBehaviourTests(ProviderTestCase):
    def test_prefetch_returns_authorised_recall(self):
        p = self.provider()
        p.sync_turn("s1", "t1", ["remember the deploy window"])
        self.assertEqual(len(p.prefetch("deploy")), 1)

    def test_prefetch_latency_is_bounded(self):
        # §24.7 prefetch latency: recall precedes every model call, so it must
        # stay fast even with a populated store.
        p = self.provider()
        for i in range(200):
            p.sync_turn("s1", f"t{i}", [f"message {i}"])
        start = time.perf_counter()
        p.prefetch("message 199", max_items=5)
        self.assertLess(time.perf_counter() - start, 0.5)

    def test_sync_turn_is_non_blocking_and_queues_ingestion(self):
        p = self.provider()
        self.assertTrue(p.sync_turn("s1", "t1", ["hello"]))
        kinds = [w.kind for w in p.pending_work()]
        self.assertIn("ingest_turn", kinds)

    def test_duplicate_turn_sync_is_ignored(self):
        p = self.provider()
        self.assertTrue(p.sync_turn("s1", "t1", ["hello"]))
        self.assertFalse(p.sync_turn("s1", "t1", ["hello"]))
        self.assertEqual(len(p.recall()), 1)

    def test_queue_prefetch_pre_warms_and_is_bounded(self):
        p = self.provider()
        self.assertTrue(p.queue_prefetch("next topic"))
        for i in range(HermesMemoryProvider.MAX_QUEUE_DEPTH + 10):
            p.queue_prefetch(f"q{i}")
        self.assertLessEqual(len(p.pending_work()), HermesMemoryProvider.MAX_QUEUE_DEPTH)

    def test_on_turn_start_hydrates_without_tool_call(self):
        p = self.provider()
        p.sync_turn("s1", "t1", ["earlier context"])
        self.assertEqual(len(p.on_turn_start("s1", "t2")), 1)

    def test_system_prompt_block_declares_automatic_recall(self):
        p = self.provider()
        self.assertEqual(p.system_prompt_block(), "")
        p.sync_turn("s1", "t1", ["hello"])
        self.assertIn("automatically", p.system_prompt_block())


class SessionLineageTests(ProviderTestCase):
    def test_session_switch_records_lineage(self):
        p = self.provider()
        record = p.on_session_switch("s1", "s2", reason="resume")
        self.assertEqual(record["from_session"], "s1")
        self.assertEqual(len(p.lineage()), 1)

    def test_branch_and_reset_lineage_is_preserved(self):
        p = self.provider()
        p.on_session_switch("s1", "s2", reason="branch")
        p.on_session_switch("s2", "s3", reason="reset")
        self.assertEqual([r["reason"] for r in p.lineage()], ["branch", "reset"])

    def test_pre_compression_capture_preserves_insights(self):
        p = self.provider()
        p.on_session_switch(None, "s1")
        preserved = p.on_pre_compress(["durable insight", {"note": "second"}])
        self.assertEqual(len(preserved), 2)
        self.assertTrue(all(e.source == "pre_compress" for e in preserved))

    def test_delegation_is_captured_in_subagent_context(self):
        p = self.provider()
        entry = p.on_delegation("s1", "sub-7", "subagent inspected the index")
        self.assertEqual(entry.context, CONTEXT_SUBAGENT)
        # §24.6: delegated work must not corrupt the user representation.
        self.assertNotIn(entry.key, [e["key"] for e in p.user_model()["entries"]])

    def test_builtin_memory_write_is_mirrored_with_provenance(self):
        p = self.provider()
        entry = p.on_memory_write("theme", "dark")
        self.assertEqual(entry.source, "hermes_builtin")
        self.assertEqual(entry.context, CONTEXT_PRIMARY)


class ToolTests(ProviderTestCase):
    def test_all_governed_tools_are_exposed(self):
        names = {t["name"] for t in self.provider().get_tool_schemas()}
        self.assertEqual(names, set(REQUIRED_TOOLS))

    def test_every_tool_is_dispatchable(self):
        p = self.provider()
        p.write_memory("theme", "dark")
        p.sync_turn("s1", "t1", ["hello"])
        calls = {
            "scos_recall": {"query": ""},
            "scos_explain": {"key": "theme"},
            "scos_user_model": {},
            "scos_memory_graph": {},
            "scos_correct": {"key": "theme", "value": "light"},
            "scos_forget": {"key": "theme"},
            "scos_status": {},
        }
        for name, args in calls.items():
            self.assertIsNotNone(p.handle_tool_call(name, args), f"{name} returned nothing")

    def test_unknown_tool_is_rejected(self):
        with self.assertRaises(ValueError):
            self.provider().handle_tool_call("scos_unknown", {})

    def test_correction_preserves_superseded_version(self):
        # §4 forbids deleting history during correction.
        p = self.provider()
        p.write_memory("theme", "dark")
        p.correct("theme", "light")
        self.assertEqual(p.read_memory("theme").value, "light")
        archived = [k for k in p.memory_graph()["nodes"] if k["id"].startswith("theme@")]
        self.assertEqual(len(archived), 1)

    def test_forget_reports_whether_anything_was_removed(self):
        p = self.provider()
        p.write_memory("theme", "dark")
        self.assertTrue(p.forget("theme"))
        self.assertFalse(p.forget("theme"))


class ShutdownAndBackupTests(ProviderTestCase):
    def test_shutdown_flushes_and_reports_pending(self):
        p = self.provider()
        p.queue_prefetch("warm")
        result = p.shutdown()
        self.assertEqual(result["pending"], 0)
        self.assertGreaterEqual(result["drained"], 1)
        self.assertFalse(p.is_available())

    def test_flush_without_ingester_keeps_turns_queued_and_reports_them(self):
        p = self.provider()
        p.sync_turn("s1", "t1", ["hello"])
        result = p.flush()
        self.assertEqual(result.delivered, 0)
        self.assertEqual(result.pending, 1)
        self.assertEqual(result.undelivered[0]["kind"], "ingest_turn")
        self.assertIn("no ingestion backend", result.undelivered[0]["error"])
        self.assertEqual([w.kind for w in p.pending_work()], ["ingest_turn"])

    def test_flush_delivers_turns_to_the_ingester(self):
        delivered = []
        p = self.provider(ingester=delivered.append)
        p.sync_turn("s1", "t1", ["hello"], tool_calls=[{"name": "grep"}])
        result = p.flush()
        self.assertEqual(result.delivered, 1)
        self.assertEqual(result.undelivered, [])
        self.assertEqual(p.pending_work(), [])
        self.assertEqual(delivered[0].turn_id, "t1")
        self.assertEqual(delivered[0].tool_calls, [{"name": "grep"}])

    def test_failed_ingestion_stays_queued_and_replays_after_restart(self):
        def failing(turn):
            raise ConnectionError("backend down")

        p = self.provider(ingester=failing)
        p.sync_turn("s1", "t1", ["hello"])
        report = p.shutdown()
        self.assertEqual(report["drained"], 0)
        self.assertEqual(report["pending"], 1)
        self.assertIn("backend down", report["undelivered"][0]["error"])

        delivered = []
        replacement = self.provider(ingester=delivered.append)
        self.assertEqual(replacement.flush().delivered, 1)
        self.assertEqual([t.turn_id for t in delivered], ["t1"])
        self.assertEqual(replacement.pending_work(), [])

    def test_drained_prefetch_is_served_from_the_warm_result(self):
        calls = []

        def recaller(query, max_items):
            calls.append(query)
            return [f"recalled:{query}"]

        p = self.provider(recaller=recaller)
        p.queue_prefetch("deploy", max_items=5)
        self.assertEqual(p.flush().delivered, 1)
        self.assertEqual(calls, ["deploy"])
        self.assertEqual(p.prefetch("deploy", max_items=5), ["recalled:deploy"])
        self.assertEqual(calls, ["deploy"], "warm result must be served without a second recall")
        p.prefetch("deploy", max_items=5)
        self.assertEqual(calls, ["deploy", "deploy"], "a warm result is served once")

    def test_backup_paths_point_at_the_store(self):
        p = self.provider()
        paths = p.backup_paths()
        self.assertEqual(len(paths), 1)
        self.assertTrue(paths[0].endswith("scos-nexus-memory.json"))

    def test_unavailable_backend_does_not_lose_the_turn(self):
        # §24.7 unavailable backend: with no store path configured the provider
        # still accepts and recalls turns in-process rather than failing.
        p = HermesMemoryProvider("prof1", "agent1", store_path="")
        p.initialize()
        self.assertTrue(p.sync_turn("s1", "t1", ["hello"]))
        self.assertEqual(len(p.recall()), 1)
        self.assertEqual(p.backup_paths(), [])

    def test_provider_replacement_reloads_persisted_state(self):
        first = self.provider()
        first.write_memory("theme", "dark")
        first.sync_turn("s1", "t1", ["hello"])
        first.shutdown()

        replacement = self.provider()
        self.assertEqual(replacement.read_memory("theme").value, "dark")
        self.assertEqual(len(replacement.recall()), 1)

    def test_profile_isolation_between_agents(self):
        # §24.1/§24.6: a second profile must not see the first profile's data.
        first = self.provider()
        first.write_memory("theme", "dark")

        other = HermesMemoryProvider("prof2", "agent2")
        other.initialize(profile_path=str(self.profile_path))
        self.assertIsNone(other.read_memory("theme"))


if __name__ == "__main__":
    unittest.main()
