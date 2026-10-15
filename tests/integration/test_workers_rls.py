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
        x("INSERT INTO turns (id, session_id, sequence, actor_id, status) VALUES (%s, %s, 1, %s, 'completed')",
          (turn, self.session, self.principal))
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


if __name__ == "__main__":
    unittest.main()
