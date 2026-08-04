import unittest
from datetime import datetime, timedelta, timezone
from unittest.mock import MagicMock, call

from main import (
    compute_retention_cutoff,
    embedding_backlog_count,
    expire_by_explicit_expiry,
    expire_by_retention_policy,
    resolve_retention_ttl_days,
    run_maintenance_cycle,
    vacuum_analyze,
)


def fake_cursor(rows=None, one=None):
    cur = MagicMock()
    cur.fetchall.return_value = rows or []
    cur.fetchone.return_value = one
    return cur


class RetentionHelperTests(unittest.TestCase):
    def test_resolve_ttl_valid(self):
        self.assertEqual(resolve_retention_ttl_days({"ttl_days": 30}), 30)

    def test_resolve_ttl_missing(self):
        self.assertIsNone(resolve_retention_ttl_days({}))

    def test_resolve_ttl_rejects_non_positive(self):
        self.assertIsNone(resolve_retention_ttl_days({"ttl_days": 0}))
        self.assertIsNone(resolve_retention_ttl_days({"ttl_days": -5}))

    def test_resolve_ttl_rejects_non_int(self):
        self.assertIsNone(resolve_retention_ttl_days({"ttl_days": "30"}))
        self.assertIsNone(resolve_retention_ttl_days({"ttl_days": True}))

    def test_compute_retention_cutoff(self):
        now = datetime(2026, 8, 3, tzinfo=timezone.utc)
        self.assertEqual(compute_retention_cutoff(now, 10), now - timedelta(days=10))


class ExpireByExplicitExpiryTests(unittest.TestCase):
    def test_counts_expired_rows(self):
        conn = MagicMock()
        conn.execute.return_value = fake_cursor(rows=[("a",), ("b",)])
        now = datetime.now(timezone.utc)
        count = expire_by_explicit_expiry(conn, now)
        self.assertEqual(count, 2)
        conn.execute.assert_called_once()

    def test_no_expired_rows(self):
        conn = MagicMock()
        conn.execute.return_value = fake_cursor(rows=[])
        self.assertEqual(expire_by_explicit_expiry(conn, datetime.now(timezone.utc)), 0)


class ExpireByRetentionPolicyTests(unittest.TestCase):
    def test_skips_rules_without_ttl(self):
        conn = MagicMock()
        conn.execute.side_effect = [
            fake_cursor(rows=[("tenant-1", None, {})]),
        ]
        total = expire_by_retention_policy(conn, datetime.now(timezone.utc))
        self.assertEqual(total, 0)
        conn.execute.assert_called_once()

    def test_applies_ttl_rule_and_sums_across_rules(self):
        conn = MagicMock()
        conn.execute.side_effect = [
            fake_cursor(
                rows=[
                    ("tenant-1", None, {"ttl_days": 90}),
                    ("tenant-1", "ns-1", {"ttl_days": 30}),
                ]
            ),
            fake_cursor(rows=[("m1",), ("m2",)]),
            fake_cursor(rows=[("m3",)]),
        ]
        total = expire_by_retention_policy(conn, datetime.now(timezone.utc))
        self.assertEqual(total, 3)
        self.assertEqual(conn.execute.call_count, 3)


class EmbeddingBacklogTests(unittest.TestCase):
    def test_returns_count(self):
        conn = MagicMock()
        conn.execute.return_value = fake_cursor(one=(7,))
        self.assertEqual(embedding_backlog_count(conn), 7)


class VacuumAnalyzeTests(unittest.TestCase):
    def test_vacuums_every_table(self):
        conn = MagicMock()
        tables = ("memory_embeddings", "memory_versions")
        result = vacuum_analyze(conn, tables)
        self.assertEqual(result, list(tables))
        conn.execute.assert_has_calls(
            [call("VACUUM ANALYZE memory_embeddings"), call("VACUUM ANALYZE memory_versions")]
        )


class RunMaintenanceCycleTests(unittest.TestCase):
    def test_cycle_aggregates_real_counts(self):
        conn = MagicMock()
        conn.execute.side_effect = [
            fake_cursor(rows=[("a",)]),  # explicit expiry
            fake_cursor(rows=[]),  # retention_rules (none configured)
            fake_cursor(one=(3,)),  # embedding backlog
            fake_cursor(),  # vacuum memory_embeddings
            fake_cursor(),  # vacuum memory_versions
            fake_cursor(),  # vacuum memory_items
        ]
        result = run_maintenance_cycle(conn)
        self.assertEqual(result["status"], "success")
        self.assertEqual(result["stale_items_pruned"], 1)
        self.assertEqual(result["embedding_backlog"], 3)
        self.assertEqual(
            result["tables_vacuumed"], ["memory_embeddings", "memory_versions", "memory_items"]
        )
        self.assertIn("timestamp", result)


if __name__ == "__main__":
    unittest.main()
