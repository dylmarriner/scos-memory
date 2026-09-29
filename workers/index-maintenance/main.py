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

import telemetry

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
    """Apply retention_rules to active items that carry no explicit expires_at.

    retention_rules is versioned per (tenant_id, namespace_id, resource_type):
    only the latest version of each rule is in force. The most specific rule
    governs an item -- a namespace rule over the tenant-wide rule
    (namespace_id NULL) -- so a namespace's own rule, even one with no TTL
    (keep indefinitely), is never overridden by a broader, shorter one.
    """
    rules = conn.execute(
        """
        SELECT DISTINCT ON (tenant_id, namespace_id) tenant_id, namespace_id, policy
          FROM retention_rules
         WHERE resource_type = 'memory_items'
         ORDER BY tenant_id, namespace_id, version DESC
        """
    ).fetchall()

    governed_namespaces = [ns for _, ns, _ in rules if ns is not None]

    total = 0
    for tenant_id, namespace_id, policy in rules:
        ttl_days = resolve_retention_ttl_days(policy or {})
        if ttl_days is None:
            continue
        cutoff = compute_retention_cutoff(now, ttl_days)
        # tenant_id is NOT NULL on retention_rules; namespace_id NULL marks
        # the tenant-wide rule, which skips namespaces governed by their own.
        cur = conn.execute(
            """
            UPDATE memory_items
               SET status = 'expired'
             WHERE status = 'active'
               AND expires_at IS NULL
               AND created_at < %s
               AND tenant_id = %s
               AND CASE WHEN %s::uuid IS NULL THEN namespace_id <> ALL(%s::uuid[])
                        ELSE namespace_id = %s END
            RETURNING id
            """,
            (cutoff, tenant_id, namespace_id, governed_namespaces, namespace_id),
        )
        total += len(cur.fetchall())
    return total


def embedding_backlog_count(conn: psycopg.Connection) -> int:
    """Count active memory versions with no embedding in the current text space.

    Embedding spaces are explicit and old spaces keep their vectors, so an
    embedding in any space is not enough: after a model change the backlog is
    everything the newest space (highest generation) has not covered yet.
    """
    row = conn.execute(
        """
        WITH current_space AS (
            SELECT id FROM embedding_spaces
             WHERE role = 'text'
             ORDER BY generation DESC, created_at DESC
             LIMIT 1
        )
        SELECT count(*)
          FROM memory_items mi
          JOIN memory_versions mv ON mv.id = mi.current_version_id
         WHERE mi.status = 'active'
           AND NOT EXISTS (
                 SELECT 1 FROM memory_embeddings me
                  WHERE me.memory_version_id = mv.id
                    AND me.embedding_space_id = (SELECT id FROM current_space)
           )
        """
    ).fetchone()
    return int(row[0])


def vacuum_analyze(conn: psycopg.Connection, tables: tuple[str, ...]) -> list[str]:
    """Run VACUUM ANALYZE on each maintained table and return those actually vacuumed.

    Requires an autocommit connection. PostgreSQL does not raise when the
    role lacks the MAINTAIN privilege (or table ownership): it emits a
    "permission denied to vacuum ..., skipping it" notice and moves on, so
    the notice -- not the absence of an exception -- decides the outcome.
    """
    skipped: list[str] = []

    def on_notice(diag: Any) -> None:
        message = getattr(diag, "message_primary", "") or ""
        if "skipping" in message:
            skipped.append(message)

    conn.add_notice_handler(on_notice)
    vacuumed = []
    try:
        for table in tables:
            before = len(skipped)
            conn.execute(f"VACUUM ANALYZE {table}")
            if len(skipped) == before:
                vacuumed.append(table)
            else:
                logger.warning("VACUUM ANALYZE %s skipped: %s", table, skipped[-1])
    finally:
        conn.remove_notice_handler(on_notice)
    return vacuumed


def run_maintenance_cycle(conn: psycopg.Connection) -> dict:
    """Execute one index maintenance and retention cycle."""
    now = datetime.now(timezone.utc)
    logger.info("starting index maintenance cycle")

    expired_explicit = expire_by_explicit_expiry(conn, now)
    expired_retention = expire_by_retention_policy(conn, now)
    backlog = embedding_backlog_count(conn)
    telemetry.emit("index-maintenance", [telemetry.Point(telemetry.EMBEDDING_BACKLOG, backlog, "count")])
    vacuumed = vacuum_analyze(conn, MAINTAINED_TABLES)

    return {
        "status": "success",
        "stale_items_pruned": expired_explicit + expired_retention,
        "embedding_backlog": backlog,
        "tables_vacuumed": vacuumed,
        "tables_skipped": [t for t in MAINTAINED_TABLES if t not in vacuumed],
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
