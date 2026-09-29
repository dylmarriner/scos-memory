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
from collections import Counter
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
from . import hooks, telemetry

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


# Actor recorded on lifecycle events this worker raises on its own.
SERVICE_ACTOR = "service:" + hooks.SERVICE


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
    """Claim and process up to MAX_JOBS_PER_CYCLE outbox jobs."""
    jobs = _claim_jobs(conn)
    # Commit the claim on its own so the lease and attempts increment persist
    # even if every job below fails -- otherwise a poison job would never
    # accumulate attempts and could never reach dead_letter.
    conn.commit()
    if not jobs:
        return

    tally: Counter = Counter()
    for job in jobs:
        # §22.6 events the job raises are held until its writes commit, so a
        # rolled-back job never announces a candidate or promotion.
        job_events: list[hooks.Event] = []
        try:
            # A savepoint per job: a failure rolls back only this job's
            # partial writes and leaves the transaction usable for
            # _mark_failed and for the jobs after it.
            with conn.transaction():
                outcome = _process_job(conn, job, job_events)
            tally += outcome
        except Exception:
            logger.exception("job %s failed", job["id"])
            job_events = []
            _mark_failed(conn, job)
        conn.commit()
        hooks.notify(job_events)
    _emit_consolidation_rates(tally)


def _emit_consolidation_rates(tally: Counter) -> None:
    """§32 consolidation rates over the candidates this cycle committed.

    Nothing is emitted for a cycle with no candidates: a rate over zero
    decisions is undefined, not 0%.
    """
    candidates = tally["candidates"]
    if not candidates:
        return
    telemetry.emit("consolidation", [
        telemetry.Point(telemetry.PROMOTION_RATE, tally["promoted"] / candidates, "ratio"),
        telemetry.Point(telemetry.DUPLICATE_RATE, tally["duplicates"] / candidates, "ratio"),
        telemetry.Point(telemetry.CONFLICT_RATE, tally["conflicted"] / candidates, "ratio"),
    ])


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


def _process_job(
    conn: psycopg.Connection,
    job: dict[str, Any],
    events: list[hooks.Event] | None = None,
) -> Counter:
    """Process one outbox job end-to-end; returns per-candidate outcome counts.

    Lifecycle events are appended to ``events`` for the caller to send once
    this job's transaction has committed -- never from inside it.
    """
    outcome: Counter = Counter()
    if events is None:
        events = []
    job_id = job["id"]
    tenant_id = job["tenant_id"] if isinstance(job["tenant_id"], UUID) else UUID(str(job["tenant_id"]))
    payload = json.loads(job["payload"]) if isinstance(job["payload"], str) else job["payload"]

    turn_id = payload.get("turn_id")
    if not turn_id:
        logger.warning("job %s: no turn_id in payload", job_id)
        _mark_failed(conn, job)
        return outcome

    logger.info("processing turn %s", turn_id)

    # ── 1. Load turn event and session messages ──────────────────────
    event = _load_event_for_turn(conn, tenant_id, turn_id)
    if not event:
        logger.error("job %s: no completion event for turn %s", job_id, turn_id)
        _mark_failed(conn, job)
        return outcome

    messages = _load_session_messages(conn, tenant_id, event.get("session_id"), event.get("aggregate_id"))

    # ── 2. Extract candidates ────────────────────────────────────────
    candidates = extract_candidates(event, messages)
    if not candidates:
        logger.info("turn %s: no extractable candidates", turn_id)
        _mark_completed(conn, job_id)
        return outcome

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
    # The owning principal of the turn these candidates came from; events
    # carry ids and classification only, never memory content.
    principal_id = str(candidates[0].metadata.get("actor_id") or event.get("actor_id") or "")
    persisted: list[str] = []
    promoted: list[str] = []
    for result in dedup_results:
        if not result.candidate:
            continue

        candidate = result.candidate
        persisted.append(str(candidate.candidate_id))
        candidate_conflicts = [c for c in conflicts
                               if str(candidate.source_event_id) in c.subjects]
        outcome["candidates"] += 1
        if result.action in ("merged", "superseded"):
            outcome["duplicates"] += 1
        if candidate_conflicts:
            outcome["conflicted"] += 1

        # §16: a duplicate merges its evidence into the memory it matches
        # instead of becoming another active copy of the same fact.
        if result.action == "merged" and result.merged_into:
            _write_candidate(conn, candidate, status="merged")
            _record_merge(conn, candidate, result.merged_into)
            logger.info("candidate %s: merged into %s",
                        candidate.candidate_id, result.merged_into.get("id"))
            continue

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

        supersedes = result.superseded_by if result.action == "superseded" else None

        if needs_review:
            _write_candidate(conn, candidate, status="pending_review")
            if supersedes:
                # The reviewer must see which active memory approval retires.
                _annotate_candidate(conn, candidate, {"supersedes": str(supersedes.get("id", ""))})
            logger.info("candidate %s: pending human review (%s)",
                        candidate.candidate_id, promotion.reason)
        else:
            # ── 8. Promote to active memory ──────────────────────────
            _promote_candidate(conn, candidate, promotion)
            promoted.append(str(candidate.candidate_id))
            outcome["promoted"] += 1
            if supersedes:
                _supersede_existing(conn, candidate, supersedes)
            logger.info("candidate %s: promoted to %s",
                        candidate.candidate_id, promotion.target_status)

    # ── 9. Persist conflict sets ──────────────────────────────────────
    for conflict in conflicts:
        _write_conflict(conn, conflict)

    _mark_completed(conn, job_id)
    if principal_id:
        common = {"namespace_id": str(namespace_id), "actor": SERVICE_ACTOR, "principal_id": principal_id,
                  "turn_id": str(turn_id)}
        if persisted:
            events.append(hooks.Event(hooks.ON_MEMORY_CANDIDATE, str(tenant_id), principal_id,
                                      {**common, "memory_ids": persisted, "change_type": "extracted"}))
        if promoted:
            events.append(hooks.Event(hooks.ON_MEMORY_PROMOTED, str(tenant_id), principal_id,
                                      {**common, "memory_ids": promoted, "change_type": "promoted"}))
    elif persisted:
        logger.warning("turn %s: no owning principal; lifecycle hooks not raised", turn_id)
    logger.info("turn %s: consolidation complete (%d candidates, %d conflicts)",
                turn_id, len(candidates), len(conflicts))
    return outcome


# ── database helpers ──────────────────────────────────────────────────────


def _load_event_for_turn(
    conn: psycopg.Connection, tenant_id: UUID, turn_id: str
) -> dict[str, Any] | None:
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(
            """
            SELECT e.id, e.tenant_id, e.aggregate_id, e.event_type, e.subject,
                   COALESCE(e.actor_id, s.created_by) AS actor_id,
                   e.occurred_at, e.data, t.session_id, s.namespace_id
            FROM events e
            JOIN turns t ON t.id = e.aggregate_id
            JOIN sessions s ON s.id = t.session_id AND s.tenant_id = e.tenant_id
            WHERE e.tenant_id = %s
              AND e.aggregate_id = %s
              AND e.event_type IN ('turn.completed', 'turn.message_accepted')
            ORDER BY e.aggregate_sequence DESC
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
            SELECT mi.id, mi.title,
                   COALESCE(mv.content, mi.canonical_summary) AS content,
                   mi.authority_level, mi.confidence_score,
                   mi.sensitivity, mi.status, mi.metadata
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


def _record_merge(
    conn: psycopg.Connection,
    candidate: MemoryCandidate,
    existing: dict[str, Any],
) -> None:
    """Record that a candidate was merged into an existing memory item.

    The candidate's evidence is attached to the existing memory as durable
    provenance, skipping references that memory already cites.
    """
    memory_id = existing.get("id")
    _annotate_candidate(conn, candidate, {
        "merged_into": str(memory_id or ""),
        "merged_by": "consolidation-worker",
        "merged_at": datetime.now(timezone.utc).isoformat(),
    }, status="merged")
    if not memory_id:
        return
    with conn.cursor() as cur:
        for evidence in candidate.evidence:
            cur.execute(
                """
                INSERT INTO memory_evidence
                    (memory_id, evidence_kind, reference_id, content_hash)
                SELECT %s, %s, %s, %s
                WHERE NOT EXISTS (
                    SELECT 1 FROM memory_evidence
                    WHERE memory_id = %s AND evidence_kind = %s AND reference_id = %s
                )
                """,
                (
                    memory_id, evidence.kind, evidence.reference_id, evidence.content_hash,
                    memory_id, evidence.kind, evidence.reference_id,
                ),
            )


def _annotate_candidate(
    conn: psycopg.Connection,
    candidate: MemoryCandidate,
    assessment: dict[str, Any],
    status: str | None = None,
) -> None:
    """Merge keys into a candidate's conflict_assessment, optionally setting status."""
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE memory_candidates
            SET status = COALESCE(%s, status),
                conflict_assessment = COALESCE(conflict_assessment, '{}'::jsonb) || %s::jsonb
            WHERE id = %s
            """,
            (status, json.dumps(assessment), candidate.candidate_id),
        )


def _supersede_existing(
    conn: psycopg.Connection,
    candidate: MemoryCandidate,
    existing: dict[str, Any],
) -> None:
    """Retire the lower-authority memory the promoted candidate replaces.

    Same convention as memory-intelligence's correctFact: the old row stays
    for audit, marked 'superseded' and pointing at its replacement, with an
    explicit 'supersedes' relation for the memory graph.
    """
    old_id = existing.get("id")
    if not old_id:
        return
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE memory_items
            SET status = 'superseded', superseded_by_id = %s,
                belief_expired_at = COALESCE(belief_expired_at, now())
            WHERE id = %s AND tenant_id = %s AND status = 'active'
            """,
            (candidate.candidate_id, old_id, candidate.tenant_id),
        )
        cur.execute(
            """
            INSERT INTO memory_relations (source_memory_id, relation_type, target_memory_id, confidence_score)
            VALUES (%s, 'supersedes', %s, %s)
            ON CONFLICT (source_memory_id, relation_type, target_memory_id) DO NOTHING
            """,
            (candidate.candidate_id, old_id, candidate.confidence_score),
        )


def _write_conflict(conn: psycopg.Connection, conflict: ConflictSet) -> None:
    """Record a conflict set as structured metadata on affected candidates.

    ``subjects`` holds the incoming candidate's source event id and the id of
    the existing memory it conflicts with (memory_items.id is the id of the
    candidate it was promoted from), so both sides are matched here.
    """
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE memory_candidates
            SET conflict_assessment = COALESCE(conflict_assessment, '{}'::jsonb) || %s::jsonb
            WHERE source_event_id::text = ANY(%s) OR id::text = ANY(%s)
            """,
            (
                json.dumps({"conflict": conflict.to_dict()}),
                list(conflict.subjects),
                list(conflict.subjects),
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
