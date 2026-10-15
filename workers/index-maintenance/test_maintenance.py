import sys
import unittest
from datetime import datetime, timezone
from unittest.mock import MagicMock

# The worker talks to PostgreSQL through psycopg, but every unit under test
# takes an injected connection. Stub the driver so the suite runs without it.
sys.modules.setdefault("psycopg", MagicMock())

from main import (  # noqa: E402
    embedding_backlog_count,
    expire_memories,
    run_maintenance_cycle,
    table_maintenance_status,
)


def fake_cursor(rows=None, one=None):
    cur = MagicMock()
    cur.fetchall.return_value = rows or []
    cur.fetchone.return_value = one
    return cur


class ExpiryTests(unittest.TestCase):
    def test_expiry_runs_through_the_definer_function(self):
        conn = MagicMock()
        conn.execute.return_value = fake_cursor(one=(2, 5))
        now = datetime.now(timezone.utc)
        self.assertEqual(expire_memories(conn, now), (2, 5))
        sql, params = conn.execute.call_args[0]
        self.assertIn("scos_worker_expire_memories", sql)
        self.assertEqual(params, (now,))


class EmbeddingBacklogTests(unittest.TestCase):
    def test_returns_count(self):
        conn = MagicMock()
        conn.execute.return_value = fake_cursor(one=(7,))
        self.assertEqual(embedding_backlog_count(conn), 7)
        self.assertIn("scos_worker_embedding_backlog", conn.execute.call_args[0][0])


class TableMaintenanceTests(unittest.TestCase):
    def test_reports_autovacuum_state(self):
        conn = MagicMock()
        ts = datetime(2026, 10, 1, tzinfo=timezone.utc)
        conn.execute.return_value = fake_cursor(rows=[("memory_items", 10, 2, ts, None)])
        status = table_maintenance_status(conn, ("memory_items",))
        self.assertEqual(status, [{"table": "memory_items", "live_rows": 10, "dead_rows": 2,
                                   "last_vacuum": ts.isoformat(), "last_analyze": None}])


class RunMaintenanceCycleTests(unittest.TestCase):
    def test_cycle_aggregates_real_counts(self):
        conn = MagicMock()
        conn.execute.side_effect = [
            fake_cursor(one=(1, 2)),  # expiry
            fake_cursor(one=(3,)),  # embedding backlog
            fake_cursor(rows=[]),  # table stats
        ]
        result = run_maintenance_cycle(conn)
        self.assertEqual(result["status"], "success")
        self.assertEqual(result["stale_items_pruned"], 3)
        self.assertEqual((result["expired_explicit"], result["expired_retention"]), (1, 2))
        self.assertEqual(result["embedding_backlog"], 3)
        self.assertEqual(result["table_maintenance"], [])
        self.assertIn("timestamp", result)


if __name__ == "__main__":
    unittest.main()
