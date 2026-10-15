#!/usr/bin/env python3
"""Policy evaluation worker (§37).

Shadow-evaluates the routing control plane's most recent policy version
against its predecessor using recorded routing_decisions outcomes, and
records a promote/rollback/hold recommendation as a governed audit entry.
A policy with too few observed outcomes to judge is held, never promoted
or rolled back on fabricated confidence.

Usage:
    SCOS_DATABASE_URL=postgres://scos:change-me@localhost:5432/scos_memory \\
    python3 main.py
"""
from __future__ import annotations

import json
import logging
import os
from typing import Any, Optional

import psycopg

logger = logging.getLogger("policy_evaluation")

DATABASE_URL = os.environ.get("SCOS_DATABASE_URL", os.environ.get("DATABASE_URL", ""))

MIN_SAMPLES = int(os.environ.get("SCOS_POLICY_EVAL_MIN_SAMPLES", "20"))
REGRESSION_THRESHOLD = float(os.environ.get("SCOS_POLICY_EVAL_REGRESSION_THRESHOLD", "0.05"))
SAMPLE_LIMIT = int(os.environ.get("SCOS_POLICY_EVAL_SAMPLE_LIMIT", "200"))


# ── pure helpers (no I/O) ───────────────────────────────────────────────


def compute_success_rate(outcomes: list[dict[str, Any]]) -> Optional[float]:
    """Fraction of outcomes with a truthy 'success' field, or None if none carry one."""
    judged = [o["success"] for o in outcomes if isinstance(o, dict) and "success" in o]
    if not judged:
        return None
    return sum(1 for s in judged if s) / len(judged)


def evaluate_regression(
    baseline_rate: Optional[float],
    baseline_n: int,
    candidate_rate: Optional[float],
    candidate_n: int,
    min_samples: int = MIN_SAMPLES,
    threshold: float = REGRESSION_THRESHOLD,
) -> str:
    """Classify a candidate policy relative to its baseline.

    Returns one of: insufficient_data, regression_detected, improved, stable.
    """
    if (
        baseline_rate is None
        or candidate_rate is None
        or baseline_n < min_samples
        or candidate_n < min_samples
    ):
        return "insufficient_data"
    delta = candidate_rate - baseline_rate
    if delta <= -threshold:
        return "regression_detected"
    if delta >= threshold:
        return "improved"
    return "stable"


def recommend_action(evaluation: str) -> str:
    return {
        "insufficient_data": "hold",
        "regression_detected": "rollback",
        "improved": "promote",
        "stable": "promote",
    }[evaluation]


# ── DB-backed operations ────────────────────────────────────────────────


def list_tenant_ids(conn: psycopg.Connection) -> list[Any]:
    """Tenants with orchestration history.

    orchestration_runs is under FORCE RLS keyed on the tenant context, so the
    cross-tenant listing goes through the SECURITY DEFINER function
    (migration 000043); everything after it runs inside one tenant.
    """
    rows = conn.execute("SELECT scos_worker_policy_tenants()").fetchall()
    return [r[0] for r in rows]


def set_tenant(conn: psycopg.Connection, tenant_id: Any) -> None:
    """Bind the current transaction to one tenant for the RLS policies."""
    conn.execute("SELECT set_config('scos.tenant_id', %s, true)", (str(tenant_id),))


def list_policy_versions(conn: psycopg.Connection, tenant_id: Any) -> list[str]:
    """Policy versions for a tenant, ordered oldest-first by first observation."""
    rows = conn.execute(
        """
        SELECT policy_version
          FROM orchestration_runs
         WHERE tenant_id = %s
         GROUP BY policy_version
         ORDER BY min(started_at) ASC
        """,
        (tenant_id,),
    ).fetchall()
    return [r[0] for r in rows]


def fetch_policy_outcomes(
    conn: psycopg.Connection, tenant_id: Any, policy_version: str, limit: int = SAMPLE_LIMIT
) -> list[dict[str, Any]]:
    rows = conn.execute(
        """
        SELECT rd.actual_outcome
          FROM routing_decisions rd
          JOIN orchestration_runs orr ON orr.id = rd.orchestration_run_id
         WHERE orr.tenant_id = %s
           AND orr.policy_version = %s
           AND rd.actual_outcome ? 'success'
         ORDER BY rd.created_at DESC
         LIMIT %s
        """,
        (tenant_id, policy_version, limit),
    ).fetchall()
    return [r[0] for r in rows]


def record_evaluation(
    conn: psycopg.Connection,
    tenant_id: Any,
    policy_version: str,
    decision: str,
    details: dict[str, Any],
) -> None:
    conn.execute(
        """
        INSERT INTO audit_entries
            (tenant_id, actor_id, action, resource_type, resource_id, decision, details)
        VALUES (%s, NULL, 'policy_shadow_evaluation', 'routing_policy', %s, %s, %s)
        """,
        (tenant_id, policy_version, decision, json.dumps(details)),
    )


def evaluate_policy_candidate(
    conn: psycopg.Connection, tenant_id: Any, candidate_version: str, baseline_version: str
) -> dict[str, Any]:
    candidate_outcomes = fetch_policy_outcomes(conn, tenant_id, candidate_version)
    baseline_outcomes = fetch_policy_outcomes(conn, tenant_id, baseline_version)

    candidate_rate = compute_success_rate(candidate_outcomes)
    baseline_rate = compute_success_rate(baseline_outcomes)

    evaluation = evaluate_regression(
        baseline_rate, len(baseline_outcomes), candidate_rate, len(candidate_outcomes)
    )
    action = recommend_action(evaluation)

    details = {
        "baseline_policy_version": baseline_version,
        "baseline_success_rate": baseline_rate,
        "baseline_sample_size": len(baseline_outcomes),
        "candidate_success_rate": candidate_rate,
        "candidate_sample_size": len(candidate_outcomes),
        "evaluation": evaluation,
    }
    record_evaluation(conn, tenant_id, candidate_version, action, details)

    return {
        "tenant_id": tenant_id,
        "policy": candidate_version,
        "eval_status": evaluation,
        "recommended_action": action,
        **details,
    }


def run_evaluation_cycle(conn: psycopg.Connection) -> list[dict[str, Any]]:
    results = []
    for tenant_id in list_tenant_ids(conn):
        with conn.transaction():
            set_tenant(conn, tenant_id)
            versions = list_policy_versions(conn, tenant_id)
            if len(versions) < 2:
                continue
            baseline_version, candidate_version = versions[-2], versions[-1]
            logger.info(
                "evaluating policy %s against baseline %s for tenant %s",
                candidate_version,
                baseline_version,
                tenant_id,
            )
            results.append(
                evaluate_policy_candidate(conn, tenant_id, candidate_version, baseline_version)
            )
    return results


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    if not DATABASE_URL:
        raise SystemExit("SCOS_DATABASE_URL or DATABASE_URL must be set")

    logger.info("Policy Evaluation Worker started.")
    with psycopg.connect(DATABASE_URL, autocommit=True) as conn:
        results = run_evaluation_cycle(conn)
    logger.info("Evaluation cycle completed: %s", results)


if __name__ == "__main__":
    main()
