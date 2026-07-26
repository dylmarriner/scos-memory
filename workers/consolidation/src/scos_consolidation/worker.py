"""Consolidation worker main loop.

Polls ``outbox_jobs`` for ``turn.completed`` events, runs extraction,
dedup, conflict detection, promotion, and persists results atomically.

Usage:
    SCOS_DATABASE_URL=postgres://scos:change-me@localhost:5432/scos_memory \\
    python -m scos_consolidation.worker
"""

from __future__ import annotations

import json
import logging
import os
import time
from datetime import datetime, timedelta, timezone
from typing import Any
from uuid import UUID

import psycopg
from psycopg import sql as pysql
from psycopg.rows import dict_row

from .conflicts import ConflictSet, detect_conflicts
from .dedup import deduplicate
from .extractor import extract_candidates
from .models import MemoryCandidate
from .promoter import evaluate_promotion, build_memory_item, build_memory_version
from .review import requires_human_review, apply_review

logger = logging.getLogger(__name__)

# ── configuration ─────────────────────────────────────────────────────────

POLL_INTERVAL_SECONDS = float(os.environ.get("SCOS_CONSOLIDATION_POLL_SECONDS", "5"))
MAX_JOBS_PER_CYCLE = int(os.environ.get("SCOS_CONSOLIDATION_BATCH_SIZE", "10"))
LEASE_SECONDS = int(os.environ.get("SCOS_CONSOLIDATION_LEASE_SECONDS", "60"))
DATABASE_URL = os.environ.get(
    "SCOS_DATABASE_URL",
    os.environ.get("DATABASE_URL", ""),
)


# ── main loop ─────────────────────────────────────────────────────────────


def main() -> None:
    logging.basicConfig(
        level=getattr(logging, os.environ.get("SCOS_LOG_LEVEL", "INFO").upper(), logging.INFO),
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    )

    if not DATABASE_URL:
        raise SystemExit("SCOS_DATABASE_URL or DATABASE_URL must be set")

    logger.info("consolidation worker starting, poll_interval=%ds", POLL_INTERVAL_SECONDS)
    while True:
        try:
            with psycopg.connect(DATABASE_URL, row_factory=dict_row) as conn:
                _process_cycle(conn)
        except Exception:
            logger.exception("cycle failed")
        time.sleep(POLL_INTERVAL_SECONDS)


# ── cycle logic ───────────────────────────────────────────────────────────


def _process_cycle(conn: psycopg.Connection) -> None:
    """Claim and process up to MAX_JOBS_PER_CYCLE outbox jobs."""
    jobs = _claim_jobs(conn)
    if not jobs:
        return

    for job in jobs:
        try:
            _process_job(conn, job)
        except Exception:
            logger.exception("job %s failed", job["id"])
            _mark_failed(conn, job)

    conn.commit()


def _claim_jobs(conn: psycopg.Connection) -> list[dict[str, Any]]:
    """Claim pending outbox_jobs for turn.completed events."""
    now = datetime.now(timezone.utc)
    lease_until = now + timedelta(seconds=LEASE_SECONDS)

    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(
            """
            UPDATE outbox_jobs
            SET status = 'processing',
                lease_owner = 'consolidation-worker',
                lease_expires_at = %s,
                attempts = attempts + 1,
                updated_at = %s
            WHERE id IN (
                SELECT id FROM outbox_jobs
                WHERE status IN ('pending', 'retryable')
                  AND job_type IN ('turn.completed', 'turn.accepted')
                  AND available_at <= %s
                  AND (lease_expires_at IS NULL OR lease_expires_at < %s)
                ORDER BY available_at ASC
                LIMIT %s
                FOR UPDATE SKIP LOCKED
            )
            RETURNING id, tenant_id, operation_id, job_type, payload, attempts,
                      lease_expires_at
            """,
            (lease_until, now, now, now, MAX_JOBS_PER_CYCLE),
        )
        return cur.fetchall()


def _process_job(conn: psycopg.Connection, job: dict[str, Any]) -> None:
    """Process one outbox job end-to-end."""
    job_id = job["id"]
    tenant_id = UUID(job["tenant_id"])
    payload = json.loads(job["payload"]) if isinstance(job["payload"], str) else job["payload"]

    turn_id = payload.get("turn_id")
    if not turn_id:
        logger.warning("job %s: no turn_id in payload", job_id)
        return

    logger.info("processing turn %s", turn_id)

    # ── 1. Load turn event and session messages ──────────────────────
    event = _load_event_for_turn(conn, tenant_id, turn_id)
    if not event:
        logger.error("job %s: no completion event for turn %s", job_id, turn_id)
        _mark_failed(conn, job)
        return

    messages = _load_session_messages(conn, tenant_id, event.get("session_id"), event.get("aggregate_id"))

    # ── 2. Extract candidates ────────────────────────────────────────
    candidates = extract_candidates(event, messages)
    if not candidates:
        logger.info("turn %s: no extractable candidates", turn_id)
        _mark_completed(conn, job_id)
        return

    # ── 3. Load existing context for dedup and conflict detection ────
    namespace_id = candidates[0].namespace_id
    existing = _load_existing_memory(conn, tenant_id, namespace_id, candidates)

    # ── 4. Deduplicate ───────────────────────────────────────────────
    dedup_results = deduplicate(candidates, existing)

    # ── 5. Detect conflicts ──────────────────────────────────────────
    new_candidates = [r.candidate for r in dedup_results
                      if r.action == "new" and r.candidate is not None]
    merged_candidates = [r.candidate for r in dedup_results
                         if r.action == "merged" and r.candidate is not None]
    all_candidates = new_candidates + merged_candidates

    conflicts = detect_conflicts(all_candidates, existing)

    # ── 6. Evaluate promotion for each candidate ─────────────────────
    for result in dedup_results:
        if result.action == "superseded" or not result.candidate:
            continue

        candidate = result.candidate
        candidate_conflicts = [c for c in conflicts
                               if str(candidate.source_event_id) in c.subjects]

        promotion = evaluate_promotion(candidate, candidate_conflicts)

        if promotion.decision == "reject":
            logger.info("candidate %s: rejected (%s)", candidate.candidate_id, promotion.reason)
            _write_candidate(conn, candidate, status="rejected")
            continue

        # ── 7. Check human review requirement ────────────────────────
        candidate_dict = {
            "proposed_type": candidate.memory_class.value,
            "sensitivity": candidate.sensitivity_class.value,
            "content": candidate.content,
            "title": candidate.title,
        }
        needs_review, review_reason = requires_human_review(candidate_dict)
        needs_review = needs_review or promotion.requires_human

        if needs_review:
            _write_candidate(conn, candidate, status="pending_review")
            logger.info("candidate %s: pending human review (%s)",
                        candidate.candidate_id, promotion.reason)
        else:
            # ── 8. Promote to active memory ──────────────────────────
            _promote_candidate(conn, candidate, promotion)
            logger.info("candidate %s: promoted to %s",
                        candidate.candidate_id, promotion.target_status)

        # Write dedup results for merged candidates
        if result.action == "merged" and result.merged_into:
            _record_merge(conn, candidate, result.merged_into)

    # ── 9. Persist conflict sets ──────────────────────────────────────
    for conflict in conflicts:
        _write_conflict(conn, conflict)

    _mark_completed(conn, job_id)
    logger.info("turn %s: consolidation complete (%d candidates, %d conflicts)",
                turn_id, len(candidates), len(conflicts))


# ── database helpers ──────────────────────────────────────────────────────


def _load_event_for_turn(
    conn: psycopg.Connection, tenant_id: UUID, turn_id: str
) -> dict[str, Any] | None:
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(
            """
            SELECT id, tenant_id, aggregate_id, event_type, subject,
                   actor_id, occurred_at, data
            FROM events
            WHERE tenant_id = %s
              AND aggregate_id = %s
              AND event_type IN ('turn.completed', 'turn.message_accepted')
            ORDER BY aggregate_sequence DESC
            LIMIT 1
            """,
            (tenant_id, turn_id),
        )
        return cur.fetchone()


def _load_session_messages(
    conn: psycopg.Connection,
    tenant_id: UUID,
    session_id: str | None,
    turn_id: str | None,
) -> list[dict[str, Any]]:
    if not turn_id:
        return []
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(
            """
            SELECT id, session_id, turn_id, role, content, content_hash, committed_at
            FROM messages
            WHERE tenant_id = %s
              AND session_id = %s
              AND turn_id = %s
            ORDER BY committed_at ASC
            """,
            (tenant_id, session_id, turn_id),
        )
        return cur.fetchall()


def _load_existing_memory(
    conn: psycopg.Connection,
    tenant_id: UUID,
    namespace_id: UUID,
    candidates: list[MemoryCandidate],
) -> list[dict[str, Any]]:
    """Load existing memory_items that might conflict with candidates."""
    if not candidates:
        return []

    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(
            """
            SELECT id, title, content, authority_level, confidence_score,
                   sensitivity, status, metadata
            FROM memory_items
            WHERE tenant_id = %s
              AND namespace_id = %s
              AND status = 'active'
            ORDER BY authority_level DESC, confidence_score DESC
            LIMIT 100
            """,
            (tenant_id, namespace_id),
        )
        return cur.fetchall()


def _write_candidate(
    conn: psycopg.Connection,
    candidate: MemoryCandidate,
    status: str = "pending",
) -> None:
    with conn.cursor() as cur:
        cur.execute(
            """
            INSERT INTO memory_candidates
                (id, tenant_id, namespace_id, source_event_id, proposed_type,
                 title, content, authority_level, confidence_score, sensitivity,
                 conflict_assessment, status, created_at)
            VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)
            ON CONFLICT (id) DO NOTHING
            """,
            (
                candidate.candidate_id,
                candidate.tenant_id,
                candidate.namespace_id,
                candidate.source_event_id,
                candidate.memory_class.value,
                candidate.title,
                candidate.content,
                candidate.authority_level,
                candidate.confidence_score,
                candidate.sensitivity_class.value,
                json.dumps({"evidence": [ev.model_dump() for ev in candidate.evidence]}),
                status,
                datetime.now(timezone.utc),
            ),
        )


def _promote_candidate(
    conn: psycopg.Connection,
    candidate: MemoryCandidate,
    promotion: Any,
) -> None:
    """Insert into memory_items and memory_versions atomically."""
    item = build_memory_item(candidate, promotion)
    version = build_memory_version(candidate, candidate.candidate_id)

    with conn.cursor() as cur:
        # Insert memory_item
        cur.execute(
            """
            INSERT INTO memory_items
                (id, tenant_id, namespace_id, owner_principal_id, type, status,
                 title, canonical_summary, authority_level, confidence_score,
                 sensitivity, valid_from, valid_until, expires_at, metadata)
            VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)
            ON CONFLICT (id) DO NOTHING
            """,
            (
                item["id"], item["tenant_id"], item["namespace_id"],
                item["owner_principal_id"], item["type"], item["status"],
                item["title"], item["canonical_summary"],
                item["authority_level"], item["confidence_score"],
                item["sensitivity"], item["valid_from"],
                item["valid_until"], item["expires_at"], item["metadata"],
            ),
        )

        # Insert version
        cur.execute(
            """
            INSERT INTO memory_versions
                (id, memory_id, version_number, content, structured_content,
                 content_hash, change_reason, created_by)
            VALUES (%s, %s, %s, %s, %s, %s, %s, %s)
            ON CONFLICT DO NOTHING
            """,
            (
                version["id"], version["memory_id"], version["version_number"],
                version["content"], version["structured_content"],
                version["content_hash"], version["change_reason"],
                version["created_by"],
            ),
        )

        # Link the current version
        cur.execute(
            "UPDATE memory_items SET current_version_id = %s WHERE id = %s",
            (version["id"], item["id"]),
        )

        # Also update the candidate status
        cur.execute(
            "UPDATE memory_candidates SET status = 'promoted' WHERE id = %s",
            (candidate.candidate_id,),
        )


def _record_merge(
    conn: psycopg.Connection,
    candidate: MemoryCandidate,
    existing: dict[str, Any],
) -> None:
    """Record that a candidate was merged into an existing memory item."""
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE memory_candidates
            SET status = 'merged',
                conflict_assessment = conflict_assessment || %s
            WHERE id = %s
            """,
            (
                json.dumps({
                    "merged_into": str(existing.get("id", "")),
                    "merged_by": "consolidation-worker",
                    "merged_at": datetime.now(timezone.utc).isoformat(),
                }),
                candidate.candidate_id,
            ),
        )


def _write_conflict(conn: psycopg.Connection, conflict: ConflictSet) -> None:
    """Record a conflict set as structured metadata on affected candidates."""
    conflict_data = conflict.to_dict()
    for subject in conflict.subjects:
        with conn.cursor() as cur:
            cur.execute(
                """
                UPDATE memory_candidates
                SET conflict_assessment = conflict_assessment || %s
                WHERE id = %s
                """,
                (
                    json.dumps({"conflict": conflict_data}),
                    subject,
                ),
            )


def _mark_completed(conn: psycopg.Connection, job_id: UUID) -> None:
    with conn.cursor() as cur:
        cur.execute(
            "UPDATE outbox_jobs SET status = 'succeeded', updated_at = %s WHERE id = %s",
            (datetime.now(timezone.utc), job_id),
        )


def _mark_failed(conn: psycopg.Connection, job: dict[str, Any]) -> None:
    job_id = job["id"]
    attempts = job.get("attempts", 0)
    max_attempts = 5
    new_status = "dead_letter" if attempts >= max_attempts else "retryable"

    with conn.cursor() as cur:
        cur.execute(
            "UPDATE outbox_jobs SET status = %s, updated_at = %s WHERE id = %s",
            (new_status, datetime.now(timezone.utc), job_id),
        )


if __name__ == "__main__":
    main()
