#!/usr/bin/env python3
"""Index maintenance worker (§37).

Expires memory items past their explicit expiry or retention horizon,
reports the vector embedding backlog, and reports autovacuum/analyze state
for the tables that back full-text and vector retrieval.

Usage:
    SCOS_DATABASE_URL=postgres://scos:change-me@localhost:5432/scos_memory \\
    python3 main.py
"""
from __future__ import annotations

import logging
import os
import time
from datetime import datetime, timezone
from typing import Any

import psycopg

logger = logging.getLogger("index_maintenance")

DATABASE_URL = os.environ.get("SCOS_DATABASE_URL", os.environ.get("DATABASE_URL", ""))

MAINTAINED_TABLES = ("memory_embeddings", "memory_versions", "memory_items")


# ── DB-backed operations ────────────────────────────────────────────────
#
# This worker connects as the restricted scos_app role. Expiry and the
# backlog scan are fabric-wide over tenant-scoped tables, so they run through
# the scos_worker_* SECURITY DEFINER functions (migrations/000043); queried
# directly, RLS would hide every row and the worker would silently do nothing.


def expire_memories(conn: psycopg.Connection, now: datetime) -> tuple[int, int]:
    """Expire memories past their explicit expiry or their retention rule.

    Returns (expired_by_explicit_expiry, expired_by_retention_rule).
    """
    row = conn.execute("SELECT * FROM scos_worker_expire_memories(%s)", (now,)).fetchone()
    return int(row[0]), int(row[1])


def embedding_backlog_count(conn: psycopg.Connection) -> int:
    """Active memories whose current version has no vector in the active
    text embedding space (the space the embeddings worker last activated,
    migrations/000053). A vector in any other space does not count: retrieval
    only compares vectors within the active space."""
    row = conn.execute("SELECT scos_worker_embedding_backlog()").fetchone()
    return int(row[0])


def active_embedding_space(conn: psycopg.Connection) -> dict[str, Any] | None:
    """The text embedding space the backlog is measured against, or None
    when the embeddings worker has not activated one yet (then every active
    memory is backlog)."""
    row = conn.execute(
        """
        SELECT id::text, provider, model, model_revision, dimensions, generation, activated_at
          FROM embedding_spaces
         WHERE role = 'text' AND activated_at IS NOT NULL
         ORDER BY activated_at DESC
         LIMIT 1
        """
    ).fetchone()
    if not row:
        return None
    return {
        "id": row[0],
        "provider": row[1],
        "model": row[2],
        "model_revision": row[3],
        "dimensions": int(row[4]),
        "generation": int(row[5]),
        "activated_at": row[6].isoformat() if row[6] else None,
    }


def table_maintenance_status(conn: psycopg.Connection, tables: tuple[str, ...]) -> list[dict[str, Any]]:
    """Vacuum/analyze state of the retrieval tables from pg_stat_user_tables.

    VACUUM requires table ownership, which the runtime role deliberately does
    not have; autovacuum maintains these tables, and this reports whether it
    is keeping up (dead tuples, last runs) instead of claiming a vacuum that
    was silently skipped.
    """
    rows = conn.execute(
        """
        SELECT relname, n_live_tup, n_dead_tup,
               GREATEST(last_vacuum, last_autovacuum) AS last_vacuum,
               GREATEST(last_analyze, last_autoanalyze) AS last_analyze
          FROM pg_stat_user_tables
         WHERE relname = ANY(%s)
         ORDER BY relname
        """,
        (list(tables),),
    ).fetchall()
    return [
        {
            "table": r[0],
            "live_rows": int(r[1]),
            "dead_rows": int(r[2]),
            "last_vacuum": r[3].isoformat() if r[3] else None,
            "last_analyze": r[4].isoformat() if r[4] else None,
        }
        for r in rows
    ]


def run_maintenance_cycle(conn: psycopg.Connection) -> dict:
    """Execute one index maintenance and retention cycle."""
    now = datetime.now(timezone.utc)
    logger.info("starting index maintenance cycle")

    expired_explicit, expired_retention = expire_memories(conn, now)
    backlog = embedding_backlog_count(conn)
    space = active_embedding_space(conn)
    tables = table_maintenance_status(conn, MAINTAINED_TABLES)

    return {
        "status": "success",
        "stale_items_pruned": expired_explicit + expired_retention,
        "expired_explicit": expired_explicit,
        "expired_retention": expired_retention,
        "embedding_backlog": backlog,
        "embedding_space": space,
        "table_maintenance": tables,
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
