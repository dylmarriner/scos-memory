"""Consolidation worker main loop.

Polls ``outbox_jobs`` for ``turn.completed`` events, runs extraction,
dedup, conflict detection, promotion, and persists results atomically.

Usage:
    SCOS_DATABASE_URL="${SCOS_DATABASE_URL:?set a TLS-enabled database URL first}" \
    python -m scos_consolidation.worker
"""

from __future__ import annotations

import json
import logging
import os
import time
from urllib.parse import parse_qs, urlparse
from datetime import datetime, timedelta, timezone
from typing import Any
from uuid import UUID

import psycopg
from psycopg import sql as pysql
from psycopg.rows import dict_row

from .conflicts import ConflictSet, detect_conflicts
from .dedup import deduplicate
from .extractor import extract_candidates
from .llm_enrichment import enrich_candidates
from .models import MemoryCandidate
from .promoter import evaluate_promotion, build_memory_item, build_memory_version
from .review import requires_human_review, apply_review
from .synthesis import run_synthesis_cycle

logger = logging.getLogger(__name__)

# ── configuration ─────────────────────────────────────────────────────────

POLL_INTERVAL_SECONDS = float(os.environ.get("SCOS_CONSOLIDATION_POLL_SECONDS", "5"))
MAX_JOBS_PER_CYCLE = int(os.environ.get("SCOS_CONSOLIDATION_BATCH_SIZE", "10"))
LEASE_SECONDS = int(os.environ.get("SCOS_CONSOLIDATION_LEASE_SECONDS", "60"))
DATABASE_URL = os.environ.get(
    "SCOS_DATABASE_URL",
    os.environ.get("DATABASE_URL", ""),
)

# Synthesis (Dream) runs on its own, much coarser clock than the per-job
# outbox poll: per-namespace eligibility is itself cadence-gated inside
# run_synthesis_cycle (SCOS_SYNTHESIS_CADENCE_HOURS), so this only needs to
# bound how often that check runs at all -- no reason to query every
# namespace's eligibility every 5-second poll tick.
SYNTHESIS_CHECK_INTERVAL_SECONDS = float(os.environ.get("SCOS_SYNTHESIS_CHECK_INTERVAL_SECONDS", "3600"))


def require_tls_database_url(value: str) -> None:
    parsed = urlparse(value)
    sslmodes = parse_qs(parsed.query).get("sslmode", [])
    if parsed.scheme not in {"postgres", "postgresql"} or not parsed.hostname or len(sslmodes) != 1 or sslmodes[0].lower() not in {"require", "verify-ca", "verify-full"}:
        raise SystemExit("SCOS_DATABASE_URL must be a PostgreSQL URL with sslmode=require, verify-ca, or verify-full")


# ── main loop ─────────────────────────────────────────────────────────────


def main() -> None:
    logging.basicConfig(
        level=getattr(logging, os.environ.get("SCOS_LOG_LEVEL", "INFO").upper(), logging.INFO),
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    )

    if not DATABASE_URL:
        raise SystemExit("SCOS_DATABASE_URL or DATABASE_URL must be set")
    require_tls_database_url(DATABASE_URL)

    logger.info("consolidation worker starting, poll_interval=%ds", POLL_INTERVAL_SECONDS)
    next_synthesis_check = 0.0
    while True:
        try:
            with psycopg.connect(DATABASE_URL, row_factory=dict_row) as conn:
                _process_cycle(conn)
                if time.monotonic() >= next_synthesis_check:
                    next_synthesis_check = time.monotonic() + SYNTHESIS_CHECK_INTERVAL_SECONDS
                    written = run_synthesis_cycle(conn)
                    if written:
                        logger.info("synthesis: %d pattern fact(s) written this cycle", written)
        except Exception:
            logger.exception("cycle failed")
        time.sleep(POLL_INTERVAL_SECONDS)


# ── cycle logic ───────────────────────────────────────────────────────────


def _process_cycle(conn: psycopg.Connection) -> None:
    """Claim and process up to MAX_JOBS_PER_CYCLE outbox jobs.

    Claiming is fabric-wide (scos_worker_claim_outbox_jobs); each job is then
    processed in its own transaction acting as the job's tenant and
    principal, so every read and write goes through the ordinary RLS
    policies.
    """
    jobs = _claim_jobs(conn)
    conn.commit()
    for job in jobs:
        try:
            _act_as(conn, job["tenant_id"], job.get("principal_id"))
            _process_job(conn, job)
            conn.commit()
        except Exception:
            logger.exception("job %s failed", job["id"])
            conn.rollback()
            try:
                _act_as(conn, job["tenant_id"], job.get("principal_id"))
                _mark_failed(conn, job)
                conn.commit()
            except Exception:
                logger.exception("job %s could not be marked failed", job["id"])
                conn.rollback()


def _act_as(conn: psycopg.Connection, tenant_id: Any, principal_id: Any) -> None:
    """Set the transaction-local RLS identity for the current transaction."""
    with conn.cursor() as cur:
        cur.execute(
            "SELECT set_config('scos.tenant_id', %s, true), set_config('scos.principal_id', %s, true)",
            (str(tenant_id), str(principal_id) if principal_id else ""),
        )


def _claim_jobs(conn: psycopg.Connection) -> list[dict[str, Any]]:
    """Claim pending turn jobs across tenants, with the principal each runs as."""
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(
            "SELECT * FROM scos_worker_claim_outbox_jobs(%s, %s, %s, %s)",
            ("consolidation-worker", ["turn.completed", "turn.accepted"], MAX_JOBS_PER_CYCLE, LEASE_SECONDS),
        )
        return cur.fetchall()


def _process_job(conn: psycopg.Connection, job: dict[str, Any]) -> None:
    """Process one outbox job end-to-end."""
    job_id = job["id"]
    tenant_id = UUID(str(job["tenant_id"]))
    payload = json.loads(job["payload"]) if isinstance(job["payload"], str) else job["payload"]

    turn_id = payload.get("turn_id")
    if not turn_id:
        logger.warning("job %s: no turn_id in payload", job_id)
        return

    if job.get("job_type") == "turn.accepted":
        # Acceptance is already journalled by memory-gateway; memories are
        # extracted once per turn, from its completion. Extracting here too
        # produced a second candidate for every turn.
        _mark_completed(conn, job_id)
        return

    logger.info("processing turn %s", turn_id)

    # ── 1. Load turn event and session messages ──────────────────────
    event = _load_event_for_turn(conn, tenant_id, turn_id)
    if not event:
        logger.error("job %s: no completion event for turn %s", job_id, turn_id)
        _mark_failed(conn, job)
        return

    # The completion event is keyed by turn; its session and namespace come
    # from the turn row, without which neither messages nor the memory's
    # namespace can be resolved.
    scope = _load_turn_scope(conn, tenant_id, turn_id)
    if not scope:
        logger.error("job %s: turn %s has no session", job_id, turn_id)
        _mark_failed(conn, job)
        return
    event["session_id"], event["namespace_id"] = scope

    messages = _load_session_messages(conn, tenant_id, event.get("session_id"), event.get("aggregate_id"))

    # ── 2. Extract candidates ────────────────────────────────────────
    candidates = extract_candidates(event, messages)
    if not candidates:
        logger.info("turn %s: no extractable candidates", turn_id)
        _mark_completed(conn, job_id)
        return

    # ── 2.5. Optional LLM rewrite of title/content for clarity ───────
    # No-op (returns candidates unchanged) unless DEEPSEEK_API_KEY is set;
    # never touches memory_class/authority/confidence/sensitivity -- those
    # stay governed by the deterministic heuristics above.
    candidates = enrich_candidates(candidates)

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
        if not result.candidate:
            continue
        candidate = result.candidate

        if result.action == "merged" and result.merged_into:
            # Already stored (or already awaiting review): keep the single
            # record and attach this turn's evidence to it. Never a second
            # active memory or a second review item.
            _merge_into_existing(conn, candidate, result.merged_into)
            logger.info("candidate %s: merged into %s %s", candidate.candidate_id,
                        result.merged_into.get("kind", "memory"), result.merged_into.get("id"))
            continue
        candidate_conflicts = [c for c in conflicts
                               if str(candidate.candidate_id) in c.subjects]

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
            if result.action == "supersedes" and result.supersedes and result.supersedes.get("kind") == "memory":
                _supersede_memory(conn, result.supersedes["id"], candidate.candidate_id)

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


def _load_turn_scope(
    conn: psycopg.Connection, tenant_id: UUID, turn_id: str
) -> tuple[str, str] | None:
    """(session_id, namespace_id) of a turn, or None when unknown."""
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(
            """
            SELECT t.session_id::text AS session_id, s.namespace_id::text AS namespace_id
            FROM turns t
            JOIN sessions s ON s.id = t.session_id
            WHERE t.id::text = %s AND s.tenant_id = %s
            """,
            (str(turn_id), tenant_id),
        )
        row = cur.fetchone()
    if not row:
        return None
    return row["session_id"], row["namespace_id"]


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
            SELECT 'memory' AS kind, mi.id, mi.title, COALESCE(mv.content, mi.canonical_summary) AS content,
                   mi.authority_level, mi.confidence_score, mi.sensitivity, mi.status, mi.metadata
            FROM memory_items mi
            LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
            WHERE mi.tenant_id = %s
              AND mi.namespace_id = %s
              AND mi.status = 'active'
            ORDER BY mi.authority_level DESC, mi.confidence_score DESC
            LIMIT 100
            """,
            (tenant_id, namespace_id),
        )
        rows = cur.fetchall()
        # Candidates already waiting for review count as existing too, so a
        # repeated statement does not queue a second review item.
        cur.execute(
            """
            SELECT 'candidate' AS kind, c.id, c.title, c.content, c.authority_level,
                   c.confidence_score, c.sensitivity, c.status, '{}'::jsonb AS metadata
            FROM memory_candidates c
            WHERE c.tenant_id = %s
              AND c.namespace_id = %s
              AND c.status = 'pending_review'
            ORDER BY c.created_at DESC
            LIMIT 100
            """,
            (tenant_id, namespace_id),
        )
        return rows + cur.fetchall()


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
    """Insert a promoted memory and its governance records atomically."""
    item = build_memory_item(candidate, promotion)
    version = build_memory_version(candidate, candidate.candidate_id)
    review = apply_review(
        candidate_id=candidate.candidate_id,
        decision="approved",
        rationale=promotion.reason,
    )

    # The candidate row is the parent of review records and may not have been
    # written yet for candidates that pass directly to promotion.
    _write_candidate(conn, candidate, status="pending")

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

        # Preserve each source reference as durable provenance for the memory.
        for evidence in candidate.evidence:
            cur.execute(
                """
                INSERT INTO memory_evidence
                    (memory_id, evidence_kind, reference_id, content_hash)
                VALUES (%s, %s, %s, %s)
                """,
                (
                    item["id"], evidence.kind, evidence.reference_id,
                    evidence.content_hash,
                ),
            )

        # Automatic promotion is itself an auditable approval decision. Human
        # review remains separate and is persisted by its review workflow.
        cur.execute(
            """
            INSERT INTO memory_reviews
                (id, candidate_id, reviewer_id, decision, rationale, created_at)
            VALUES (%s, %s, %s, %s, %s, %s)
            """,
            (
                review.review_id, review.candidate_id, review.reviewer_id,
                review.decision, review.rationale, review.created_at,
            ),
        )

        # Also update the candidate status
        cur.execute(
            "UPDATE memory_candidates SET status = 'promoted' WHERE id = %s",
            (candidate.candidate_id,),
        )


def _merge_into_existing(
    conn: psycopg.Connection,
    candidate: MemoryCandidate,
    existing: dict[str, Any],
) -> None:
    """Attach a duplicate's evidence to the record it duplicates."""
    _write_candidate(conn, candidate, status="merged")
    if existing.get("kind", "memory") == "memory":
        with conn.cursor() as cur:
            for evidence in candidate.evidence:
                cur.execute(
                    """
                    INSERT INTO memory_evidence (memory_id, evidence_kind, reference_id, content_hash)
                    SELECT %s, %s, %s, %s
                    WHERE NOT EXISTS (
                        SELECT 1 FROM memory_evidence
                         WHERE memory_id = %s AND evidence_kind = %s AND reference_id = %s)
                    """,
                    (existing["id"], evidence.kind, evidence.reference_id, evidence.content_hash,
                     existing["id"], evidence.kind, evidence.reference_id),
                )
    _record_merge(conn, candidate, existing)


def _supersede_memory(conn: psycopg.Connection, memory_id: Any, superseded_by: Any) -> None:
    """Retire an active memory replaced by a higher-authority statement of it."""
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE memory_items
               SET status = 'superseded',
                   metadata = metadata || jsonb_build_object('superseded_by', %s::text)
             WHERE id = %s AND status = 'active'
            """,
            (str(superseded_by), memory_id),
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


# Where a conflict is recorded for each kind of subject: a candidate's
# conflict_assessment (shown by the review queue) or an active memory's
# metadata. Both keep a list under "conflicts", so a record involved in
# several conflicts keeps them all.
_CONFLICT_TARGETS = {
    "candidate": pysql.SQL(
        "UPDATE memory_candidates SET conflict_assessment = jsonb_set("
        "conflict_assessment, '{conflicts}', "
        "COALESCE(conflict_assessment->'conflicts', '[]'::jsonb) || jsonb_build_array(%s::jsonb)) "
        "WHERE id = %s"
    ),
    "memory": pysql.SQL(
        "UPDATE memory_items SET metadata = jsonb_set("
        "metadata, '{conflicts}', "
        "COALESCE(metadata->'conflicts', '[]'::jsonb) || jsonb_build_array(%s::jsonb)) "
        "WHERE id = %s"
    ),
}


def _write_conflict(conn: psycopg.Connection, conflict: ConflictSet) -> None:
    """Record a conflict set on every record it involves.

    Each subject is the primary key of a memory_candidates or memory_items
    row. A subject that matches no row would leave the conflict recorded
    nowhere, so it fails the job instead of being silently dropped.
    """
    conflict_data = json.dumps(conflict.to_dict())
    for ref in conflict.subject_refs:
        with conn.cursor() as cur:
            cur.execute(_CONFLICT_TARGETS[ref["kind"]], (conflict_data, ref["id"]))
            if cur.rowcount != 1:
                raise RuntimeError(
                    f"conflict {conflict.conflict_id}: {ref['kind']} {ref['id']} is not a visible record"
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
