#!/usr/bin/env python3
"""Index maintenance worker (§37).

Expires memory items past their retention horizon, reports the vector
embedding backlog for the active semantic index, and runs VACUUM ANALYZE
on the tables that back full-text and vector retrieval so query planning
stays accurate as memory volume grows.

Usage:
    SCOS_DATABASE_URL=postgres://scos:change-me@localhost:5432/scos_memory \\
    python3 main.py
"""
from __future__ import annotations

import logging
import os
import time
from datetime import datetime, timedelta, timezone
from typing import Any, Optional

import psycopg

logger = logging.getLogger("index_maintenance")

DATABASE_URL = os.environ.get("SCOS_DATABASE_URL", os.environ.get("DATABASE_URL", ""))

MAINTAINED_TABLES = ("memory_embeddings", "memory_versions", "memory_items")


# ── pure helpers (no I/O) ───────────────────────────────────────────────


def resolve_retention_ttl_days(policy: dict[str, Any]) -> Optional[int]:
    """Extract a positive integer TTL in days from a retention_rules.policy payload."""
    ttl = policy.get("ttl_days")
    if not isinstance(ttl, int) or isinstance(ttl, bool) or ttl <= 0:
        return None
    return ttl


def compute_retention_cutoff(now: datetime, ttl_days: int) -> datetime:
    return now - timedelta(days=ttl_days)


# ── DB-backed operations ────────────────────────────────────────────────


def expire_by_explicit_expiry(conn: psycopg.Connection, now: datetime) -> int:
    """Move memory_items with a passed explicit expires_at into the 'expired' status."""
    cur = conn.execute(
        """
        UPDATE memory_items
           SET status = 'expired'
         WHERE status = 'active'
           AND expires_at IS NOT NULL
           AND expires_at < %s
        RETURNING id
        """,
        (now,),
    )
    return len(cur.fetchall())


def expire_by_retention_policy(conn: psycopg.Connection, now: datetime) -> int:
    """Apply namespace/tenant retention_rules to items with no explicit expires_at."""
    rules = conn.execute(
        """
        SELECT tenant_id, namespace_id, policy
          FROM retention_rules
         WHERE resource_type = 'memory_items'
        """
    ).fetchall()

    total = 0
    for tenant_id, namespace_id, policy in rules:
        ttl_days = resolve_retention_ttl_days(policy or {})
        if ttl_days is None:
            continue
        cutoff = compute_retention_cutoff(now, ttl_days)
        cur = conn.execute(
            """
            UPDATE memory_items
               SET status = 'expired'
             WHERE status = 'active'
               AND expires_at IS NULL
               AND tenant_id = %s
               AND (%s::uuid IS NULL OR namespace_id = %s)
               AND created_at < %s
            RETURNING id
            """,
            (tenant_id, namespace_id, namespace_id, cutoff),
        )
        total += len(cur.fetchall())
    return total


def embedding_backlog_count(conn: psycopg.Connection) -> int:
    """Count active memory versions with no embedding rows for the current index."""
    row = conn.execute(
        """
        SELECT count(*)
          FROM memory_items mi
          JOIN memory_versions mv ON mv.id = mi.current_version_id
         WHERE mi.status = 'active'
           AND NOT EXISTS (
                 SELECT 1 FROM memory_embeddings me
                  WHERE me.memory_version_id = mv.id
           )
        """
    ).fetchone()
    return int(row[0])


def vacuum_analyze(conn: psycopg.Connection, tables: tuple[str, ...]) -> list[str]:
    """Run VACUUM ANALYZE on each maintained table. Requires an autocommit connection."""
    vacuumed = []
    for table in tables:
        conn.execute(f"VACUUM ANALYZE {table}")
        vacuumed.append(table)
    return vacuumed


def run_maintenance_cycle(conn: psycopg.Connection) -> dict:
    """Execute one index maintenance and retention cycle."""
    now = datetime.now(timezone.utc)
    logger.info("starting index maintenance cycle")

    expired_explicit = expire_by_explicit_expiry(conn, now)
    expired_retention = expire_by_retention_policy(conn, now)
    backlog = embedding_backlog_count(conn)
    vacuumed = vacuum_analyze(conn, MAINTAINED_TABLES)

    return {
        "status": "success",
        "stale_items_pruned": expired_explicit + expired_retention,
        "embedding_backlog": backlog,
        "tables_vacuumed": vacuumed,
        "timestamp": now.isoformat(),
    }


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    if not DATABASE_URL:
        raise SystemExit("SCOS_DATABASE_URL or DATABASE_URL must be set")

    logger.info("Index Maintenance Worker started.")
    with psycopg.connect(DATABASE_URL, autocommit=True) as conn:
        result = run_maintenance_cycle(conn)
    logger.info("Cycle completed: %s", result)


if __name__ == "__main__":
    main()
