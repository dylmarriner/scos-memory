"""Worker integration tests against a real, fully migrated database.

Run with both connection strings set (see `make rls-integration-test`):
  DATABASE_URL                 the migration-owning role, used only to seed
  SCOS_APP_TEST_DATABASE_URL   the restricted runtime role (scos_app)

Each test seeds tenant data as the owner, runs one worker cycle as the
runtime role, and asserts the work actually happened under RLS.
"""
from __future__ import annotations

import hashlib
import json
import os
import sys
import unittest
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "workers/consolidation/src"))
sys.path.insert(0, str(ROOT / "workers/embeddings/src"))

import importlib.util  # noqa: E402

import psycopg  # noqa: E402
from psycopg.rows import dict_row  # noqa: E402

ADMIN_URL = os.environ.get("DATABASE_URL", "")
APP_URL = os.environ.get("SCOS_APP_TEST_DATABASE_URL", "")


def _load(name: str, path: Path):
    """Load a worker's main.py by path (several workers share that name)."""
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    assert spec and spec.loader
    spec.loader.exec_module(module)
    return module


def _require_urls() -> None:
    if not ADMIN_URL or not APP_URL:
        raise RuntimeError("DATABASE_URL (owner) and SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")


class Fixture:
    """A tenant with one principal, namespace and session."""

    def __init__(self, admin: psycopg.Connection):
        self.admin = admin
        self.tenant = str(uuid.uuid4())
        self.principal = str(uuid.uuid4())
        self.namespace = str(uuid.uuid4())
        self.session = str(uuid.uuid4())
        x = admin.execute
        x("INSERT INTO tenants (id, name) VALUES (%s, 'worker-it')", (self.tenant,))
        x("INSERT INTO principals (id, tenant_id, kind, external_id, display_name) VALUES (%s, %s, 'human', %s, 'IT')",
          (self.principal, self.tenant, "worker-it-" + self.principal))
        x("INSERT INTO namespaces (id, tenant_id, owner_principal_id, canonical_name, sensitivity_ceiling) VALUES (%s, %s, %s, 'it', 'internal')",
          (self.namespace, self.tenant, self.principal))
        x("INSERT INTO namespace_memberships (namespace_id, principal_id, can_read, can_write_candidates, can_promote, can_administer) VALUES (%s, %s, true, true, true, true)",
          (self.namespace, self.principal))
        x("INSERT INTO sessions (id, tenant_id, namespace_id, created_by) VALUES (%s, %s, %s, %s)",
          (self.session, self.tenant, self.namespace, self.principal))
        admin.commit()

    def completed_turn(self, user: str, assistant: str) -> str:
        turn = str(uuid.uuid4())
        x = self.admin.execute
        self.turns = getattr(self, "turns", 0) + 1
        x("INSERT INTO turns (id, session_id, sequence, actor_id, status) VALUES (%s, %s, %s, %s, 'completed')",
          (turn, self.session, self.turns, self.principal))
        for role, content in (("user", user), ("assistant", assistant)):
            x("INSERT INTO messages (tenant_id, session_id, turn_id, role, content, content_hash, sensitivity) VALUES (%s, %s, %s, %s, %s, %s, 'internal')",
              (self.tenant, self.session, turn, role, content, hashlib.sha256(content.encode()).hexdigest()))
        x("""INSERT INTO events (id, tenant_id, aggregate_id, aggregate_sequence, source, event_type, subject, occurred_at, data_schema, actor_id, data)
             VALUES (%s, %s, %s, 1, 'memory-gateway', 'turn.completed', %s, now(), 'scos.turn.completed/1', %s, %s)""",
          (str(uuid.uuid4()), self.tenant, turn, "turn/" + turn, self.principal, json.dumps({"session_id": self.session})))
        x("INSERT INTO outbox_jobs (tenant_id, operation_id, job_type, payload) VALUES (%s, %s, 'turn.completed', %s)",
          (self.tenant, turn + ":completed", json.dumps({"turn_id": turn, "principal_id": self.principal, "session_id": self.session})))
        self.admin.commit()
        return turn


class WorkerRLSTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        _require_urls()
        cls.admin = psycopg.connect(ADMIN_URL)
        cls.app = psycopg.connect(APP_URL, row_factory=dict_row)

    @classmethod
    def tearDownClass(cls):
        cls.app.close()
        cls.admin.close()

    def test_consolidation_processes_turns_as_the_runtime_role(self):
        from scos_consolidation import worker

        fx = Fixture(self.admin)
        turn = fx.completed_turn(
            "What database does this project use for its memory store?",
            "The project uses PostgreSQL with the pgvector extension for its memory store.",
        )
        visible = self.app.execute("SELECT count(*) AS n FROM outbox_jobs").fetchone()["n"]
        self.app.commit()
        self.assertEqual(visible, 0, "precondition: the runtime role cannot scan tenant jobs directly")

        worker._process_cycle(self.app)

        status = self.admin.execute("SELECT status::text FROM outbox_jobs WHERE operation_id = %s", (turn + ":completed",)).fetchone()[0]
        candidates = self.admin.execute("SELECT count(*) FROM memory_candidates WHERE tenant_id = %s", (fx.tenant,)).fetchone()[0]
        self.admin.commit()
        self.assertEqual(status, "succeeded")
        self.assertGreaterEqual(candidates, 1, "a completed turn must yield a memory candidate")

    def test_repeated_fact_is_one_memory_with_merged_evidence(self):
        from scos_consolidation import worker

        fx = Fixture(self.admin)
        statement = "Remember that the staging cluster runs Kubernetes 1.31."
        first = fx.completed_turn(statement, "Noted.")
        worker._process_cycle(self.app)
        second = fx.completed_turn(statement, "Noted again.")
        worker._process_cycle(self.app)

        rows = self.admin.execute(
            """SELECT count(*) FILTER (WHERE mi.status = 'active'), count(*)
                 FROM memory_items mi JOIN memory_versions mv ON mv.id = mi.current_version_id
                WHERE mi.tenant_id = %s AND mv.content = 'The staging cluster runs Kubernetes 1.31.'""",
            (fx.tenant,)).fetchone()
        statuses = sorted(r[0] for r in self.admin.execute(
            "SELECT status FROM memory_candidates WHERE tenant_id = %s", (fx.tenant,)).fetchall())
        evidence = self.admin.execute(
            """SELECT count(DISTINCT e.reference_id) FROM memory_evidence e
                 JOIN memory_items mi ON mi.id = e.memory_id WHERE mi.tenant_id = %s""",
            (fx.tenant,)).fetchone()[0]
        self.admin.commit()
        self.assertEqual(rows, (1, 1), "a repeated fact must stay one active memory")
        self.assertEqual(statuses, ["merged", "promoted"])
        self.assertGreater(evidence, 1, "the second turn's evidence must be attached to the existing memory")
        self.assertNotEqual(first, second)

    def test_conflict_is_recorded_on_the_candidate_and_the_memory_it_contradicts(self):
        from scos_consolidation import worker

        fx = Fixture(self.admin)
        memory = str(uuid.uuid4())
        version = str(uuid.uuid4())
        title = "The release train leaves on Wednesdays."
        self.admin.execute(
            """INSERT INTO memory_items (id, tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary,
                   authority_level, confidence_score, sensitivity)
               VALUES (%s, %s, %s, %s, 'semantic', 'active', %s, 'Releases are cut by hand whenever QA signs off.', 10, 0.5, 'internal')""",
            (memory, fx.tenant, fx.namespace, fx.principal, title))
        self.admin.execute(
            """INSERT INTO memory_versions (id, memory_id, version_number, content, structured_content, content_hash, change_reason, created_by)
               VALUES (%s, %s, 1, 'Releases are cut by hand whenever QA signs off.', '{}', 'h', 'seed', %s)""",
            (version, memory, fx.principal))
        self.admin.execute("UPDATE memory_items SET current_version_id = %s WHERE id = %s", (version, memory))
        self.admin.commit()

        turn = fx.completed_turn("Remember that the release train leaves on Wednesdays.", "Noted.")
        worker._process_cycle(self.app)

        status = self.admin.execute("SELECT status::text FROM outbox_jobs WHERE operation_id = %s", (turn + ":completed",)).fetchone()[0]
        candidate = self.admin.execute(
            "SELECT id::text, status, conflict_assessment FROM memory_candidates WHERE tenant_id = %s", (fx.tenant,)).fetchone()
        memory_meta = self.admin.execute("SELECT metadata FROM memory_items WHERE id = %s", (memory,)).fetchone()[0]
        self.admin.commit()
        self.assertEqual(status, "succeeded")
        candidate_id, candidate_status, assessment = candidate
        self.assertEqual(candidate_status, "pending_review", "a conflicting candidate is held for review")
        recorded = assessment.get("conflicts", [])
        self.assertEqual(len(recorded), 1, assessment)
        self.assertEqual(recorded[0]["subjects"], [candidate_id, memory])
        self.assertEqual(recorded[0]["subject_refs"], [{"kind": "candidate", "id": candidate_id}, {"kind": "memory", "id": memory}])
        self.assertEqual([c["conflict_id"] for c in memory_meta.get("conflicts", [])], [recorded[0]["conflict_id"]],
                         "the contradicted memory must carry the same conflict set")

    def test_embedding_backlog_is_measured_in_the_active_space(self):
        from scos_embeddings import worker as emb
        index_main = _load("index_maintenance_backlog", ROOT / "workers/index-maintenance/main.py")

        fx = Fixture(self.admin)
        memory, version = str(uuid.uuid4()), str(uuid.uuid4())
        self.admin.execute(
            """INSERT INTO memory_items (id, tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary,
                   authority_level, confidence_score, sensitivity)
               VALUES (%s, %s, %s, %s, 'semantic', 'active', 'space test', 'space test', 50, 0.5, 'internal')""",
            (memory, fx.tenant, fx.namespace, fx.principal))
        self.admin.execute(
            """INSERT INTO memory_versions (id, memory_id, version_number, content, structured_content, content_hash, change_reason, created_by)
               VALUES (%s, %s, 1, 'space test', '{}', 'h', 'seed', %s)""",
            (version, memory, fx.principal))
        self.admin.execute("UPDATE memory_items SET current_version_id = %s WHERE id = %s", (version, memory))
        self.admin.commit()

        def backlog_has_memory(space_id: str) -> bool:
            # The backlog is fabric-wide, so check this memory's own standing
            # against the active space rather than the global count.
            return self.admin.execute(
                """SELECT NOT EXISTS (SELECT 1 FROM memory_embeddings me
                                       WHERE me.memory_version_id = %s AND me.embedding_space_id = %s
                                         AND me.chunk_number = 0 AND me.embedding IS NOT NULL)""",
                (version, space_id)).fetchone()[0]

        tag = uuid.uuid4().hex[:8]
        app = psycopg.connect(APP_URL)
        try:
            old_space = emb._ensure_embedding_space(app, "it-provider", "it-model-" + tag, "a", 3, 1)
            emb._activate_embedding_space(app, old_space)
            app.execute("SELECT scos_worker_store_embedding(%s::uuid, %s::uuid, 'it-provider', %s, 'a', 3, 'h', '[0.1,0.2,0.3]')",
                        (version, old_space, "it-model-" + tag))
            app.commit()
            before = index_main.embedding_backlog_count(app)
            app.commit()

            # A model change: a new active space where this memory has no vector yet.
            new_space = emb._ensure_embedding_space(app, "it-provider", "it-model-" + tag, "b", 3, 1)
            self.assertTrue(emb._activate_embedding_space(app, new_space))
            self.assertFalse(emb._activate_embedding_space(app, new_space), "re-activating the active space is a no-op")
            app.commit()
            after_switch = index_main.embedding_backlog_count(app)
            active = index_main.active_embedding_space(app)
            app.commit()
            self.assertEqual(active["id"], new_space)
            self.assertTrue(backlog_has_memory(new_space))
            self.assertEqual(after_switch, before + 1,
                             "a memory embedded only in the previous space is backlog in the active one")

            app.execute("SELECT scos_worker_store_embedding(%s::uuid, %s::uuid, 'it-provider', %s, 'b', 3, 'h', '[0.3,0.2,0.1]')",
                        (version, new_space, "it-model-" + tag))
            app.commit()
            self.assertEqual(index_main.embedding_backlog_count(app), after_switch - 1)
            app.commit()
        finally:
            # Leave the database's real active space as it was.
            self.admin.execute("DELETE FROM memory_embeddings WHERE memory_version_id = %s", (version,))
            self.admin.execute("DELETE FROM embedding_spaces WHERE model = %s", ("it-model-" + tag,))
            self.admin.commit()
            app.close()

    def test_index_maintenance_expires_across_tenants(self):
        index_main = _load("index_maintenance_main", ROOT / "workers/index-maintenance/main.py")

        fx = Fixture(self.admin)
        item = str(uuid.uuid4())
        self.admin.execute(
            """INSERT INTO memory_items (id, tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary,
                   authority_level, confidence_score, sensitivity, expires_at)
               VALUES (%s, %s, %s, %s, 'working', 'active', 't', 's', 50, 0.5, 'internal', now() - interval '1 hour')""",
            (item, fx.tenant, fx.namespace, fx.principal))
        self.admin.commit()
        app = psycopg.connect(APP_URL, autocommit=True)
        try:
            result = index_main.run_maintenance_cycle(app)
        finally:
            app.close()
        status = self.admin.execute("SELECT status::text FROM memory_items WHERE id = %s", (item,)).fetchone()[0]
        self.admin.commit()
        self.assertEqual(status, "expired")
        self.assertGreaterEqual(result["stale_items_pruned"], 1)

    def test_policy_evaluation_judges_recorded_outcomes(self):
        policy_main = _load("policy_evaluation_main", ROOT / "workers/policy-evaluation/main.py")

        fx = Fixture(self.admin)
        started = datetime.now(timezone.utc) - timedelta(hours=2)
        # Baseline: every routed turn completed. Candidate: half degraded.
        for version, offset, outcomes in (
            ("policy-a", 0, [True] * 25),
            ("policy-b", 60, [True] * 12 + [False] * 13),
        ):
            for i, success in enumerate(outcomes):
                run = str(uuid.uuid4())
                self.admin.execute(
                    """INSERT INTO orchestration_runs (id, tenant_id, interaction_id, status, current_stage, policy_version, registry_generation, started_at)
                       VALUES (%s, %s, %s, %s, 'turn-coordinator', %s, 1, %s)""",
                    (run, fx.tenant, f"{version}-{i}", "completed" if success else "degraded", version,
                     started + timedelta(minutes=offset, seconds=i)))
                self.admin.execute(
                    """INSERT INTO routing_decisions (tenant_id, actor_id, orchestration_run_id, decision_id, intent_labels, resolved_scope,
                           risk_class, candidate_capabilities, selected_plan, reason_codes, confidence, policy_version, registry_generation, actual_outcome)
                       VALUES (%s, %s, %s, %s, '[]', '{}', 'low', '[]', '[]', '[]', 0.9, %s, '1', %s)""",
                    (fx.tenant, fx.principal, run, "dec-" + run, version,
                     json.dumps({"confirmation_required": False, "success": success,
                                 "status": "completed" if success else "degraded"})))
        self.admin.commit()

        app = psycopg.connect(APP_URL, autocommit=True)
        try:
            results = policy_main.run_evaluation_cycle(app)
        finally:
            app.close()
        mine = [r for r in results if str(r["tenant_id"]) == fx.tenant]
        self.assertEqual(len(mine), 1)
        self.assertEqual(mine[0]["policy"], "policy-b")
        self.assertEqual(mine[0]["baseline_sample_size"], 25)
        self.assertEqual(mine[0]["candidate_sample_size"], 25)
        self.assertEqual(mine[0]["eval_status"], "regression_detected")
        audit = self.admin.execute(
            "SELECT decision, details->>'evaluation' FROM audit_entries WHERE tenant_id = %s AND action = 'policy_shadow_evaluation'",
            (fx.tenant,)).fetchall()
        self.admin.commit()
        self.assertEqual(audit, [("rollback", "regression_detected")])


if __name__ == "__main__":
    unittest.main()
