"""Dream-style Synthesis (§16/§25 of GOVERNANCE.md, mem0's Dream feature).

Periodically distills a namespace's accumulated active facts into new
higher-order "pattern" facts -- insights that only become visible when
several facts are considered together, not a summary of any single one.
Patterns are added alongside their source facts, never in place of them.
A pattern is a model's inference, so it is not written as active memory:
it is proposed for human review (memory_candidates, pending_review, the
queue memory-intelligence's review endpoints work), carrying the facts it
cites, its derivation and the highest sensitivity among those facts.
Approving it stores an inferred fact citing them. Earlier patterns are not
fed back in as source facts, and a pattern that restates one already
stored or proposed is not proposed again.

Gated the same way mem0's Synthesis is:
- a namespace needs a minimum number of active facts before it's eligible
  at all (SCOS_SYNTHESIS_MIN_FACTS, default 20);
- each namespace is reprocessed at most once per configured cadence
  (SCOS_SYNTHESIS_CADENCE_HOURS, default 24), tracked in
  memory_synthesis_state so a slow-changing namespace isn't reconsidered on
  every consolidation-worker cycle.

There is no deterministic fallback here, unlike llm_enrichment's rewrite
pass: "find the recurring pattern across N unrelated-looking facts" is not
something a regex or word-overlap heuristic can do credibly. Disabled
entirely without DEEPSEEK_API_KEY.
"""

from __future__ import annotations

import json
import logging
import os
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timedelta, timezone

import psycopg
from psycopg.rows import dict_row

from .dedup import DUPLICATE_OVERLAP
from .statements import duplicate_score

logger = logging.getLogger(__name__)

MIN_FACTS_FOR_SYNTHESIS = int(os.environ.get("SCOS_SYNTHESIS_MIN_FACTS", "20"))
CADENCE_HOURS = float(os.environ.get("SCOS_SYNTHESIS_CADENCE_HOURS", "24"))
MAX_FACTS_PER_RUN = int(os.environ.get("SCOS_SYNTHESIS_MAX_FACTS", "200"))
MIN_SOURCE_FACTS_PER_PATTERN = 2

_SYSTEM_PROMPT = (
    "You look at a list of factual memories about the same subject and find "
    "recurring patterns that only become visible when several of them are "
    "considered together -- a higher-order insight implied by multiple "
    "facts, not a restatement or summary of any single fact. Only propose a "
    "pattern when at least two of the given facts genuinely support it; do "
    "not invent connections that aren't there, and do not propose a pattern "
    "for facts that are already saying the same single thing (that is "
    "deduplication, a different process, not synthesis). Respond with a "
    "JSON object of the exact shape "
    '{"patterns": [{"content": "the distilled higher-order fact", '
    '"source_fact_ids": ["id1", "id2"], "confidence": 0.0-1.0}]}. '
    "Return an empty patterns array if no genuine pattern emerges -- this is "
    "the expected common outcome, not a failure."
)


def _deepseek_config() -> tuple[str, str, str] | None:
    api_key = os.environ.get("DEEPSEEK_API_KEY", "").strip()
    if not api_key:
        return None
    base_url = os.environ.get("DEEPSEEK_BASE_URL", "").strip().rstrip("/") or "https://api.deepseek.com/v1"
    model = os.environ.get("DEEPSEEK_MODEL", "").strip() or "deepseek-chat"
    return api_key, base_url, model


def _parse_llm_json(content: str) -> dict:
    """Parse an LLM JSON response tolerantly.

    Models sometimes wrap the JSON in ```json fences or stray prose, and a
    hard ``json.loads`` then raises "Unterminated string" on every synthesis
    cycle (which is what made synthesis look permanently broken). We strip
    fences, try the whole string, then try progressively shorter / repaired
    variants so a single bad tail doesn't kill the run.
    """
    content = content.strip()
    # Strip markdown code fences (```json ... ``` or ``` ... ```).
    if content.startswith("```"):
        content = content.split("```", 2)[1] if content.count("```") >= 2 else content.lstrip("`")
    content = content.strip().lstrip("`").strip()
    direct = _try_parse(content)
    if direct is not None:
        return direct
    # Some models emit prose around the object or truncate the tail with junk
    # braces/brackets. Walk candidate [{...}] spans from the first '{' and try
    # progressively shorter end positions until one parses.
    start = content.find("{")
    if start != -1:
        for end in range(len(content) - 1, start, -1):
            if content[end] in "}]":
                candidate = content[start:end + 1]
                parsed = _try_parse(candidate)
                if parsed is not None:
                    return parsed
    raise ValueError("LLM returned no parseable JSON object")


def _try_parse(content: str) -> dict | None:
    try:
        obj = json.loads(content)
    except (ValueError, TypeError):
        return None
    return obj if isinstance(obj, dict) else None


def _call_deepseek(api_key: str, base_url: str, model: str, facts: list[dict], timeout: float = 120.0) -> dict:
    numbered = "\n".join(f"- id={f['id']}: {f['content']}" for f in facts)
    body = json.dumps({
        "model": model,
        "messages": [
            {"role": "system", "content": _SYSTEM_PROMPT},
            {"role": "user", "content": numbered},
        ],
        "temperature": 0.2,
        # deepseek-v4-flash is a reasoning model whose reasoning_content eats
        # the token budget; with a large fact set a low cap truncates the actual
        # JSON down to zero (finish=length, content empty). Give it generous room
        # — the trace can exceed 8k on the real 179-fact namespace.
        "max_tokens": 16000,
        "response_format": {"type": "json_object"},
    }).encode("utf-8")
    req = urllib.request.Request(
        base_url + "/chat/completions",
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
        },
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        payload = json.loads(resp.read().decode("utf-8"))
    content = payload["choices"][0]["message"]["content"]
    try:
        return _parse_llm_json(content)
    except ValueError:
        # Log the raw response so a bad model output is diagnosable instead of
        # an opaque "no parseable JSON" warning every cycle.
        logger.warning(
            "deepseek synthesis returned unparseable content (len=%d finish=%s): %r",
            len(content), payload["choices"][0].get("finish_reason"), content[:400],
        )
        raise


def _eligible_namespaces(conn: psycopg.Connection) -> list[dict]:
    """Namespaces with enough active facts, across tenants, each with the
    principal synthesis acts as (scos_worker_synthesis_namespaces)."""
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(
            "SELECT * FROM scos_worker_synthesis_namespaces(%s)",
            (MIN_FACTS_FOR_SYNTHESIS,),
        )
        rows = cur.fetchall()
    conn.commit()
    return [r for r in rows if r.get("acting_principal_id")]


def _act_as(conn: psycopg.Connection, tenant_id, principal_id) -> None:
    """Set the transaction-local RLS identity for the current transaction."""
    with conn.cursor() as cur:
        cur.execute(
            "SELECT set_config('scos.tenant_id', %s, true), set_config('scos.principal_id', %s, true)",
            (str(tenant_id), str(principal_id)),
        )


def _due_for_synthesis(conn: psycopg.Connection, namespace_id) -> bool:
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute("SELECT last_run_at FROM memory_synthesis_state WHERE namespace_id = %s", (namespace_id,))
        row = cur.fetchone()
    if row is None or row.get("last_run_at") is None:
        return True
    last_run_at = row["last_run_at"]
    if last_run_at.tzinfo is None:
        last_run_at = last_run_at.replace(tzinfo=timezone.utc)
    return datetime.now(timezone.utc) - last_run_at >= timedelta(hours=CADENCE_HOURS)


def _load_facts_for_synthesis(conn: psycopg.Connection, namespace_id) -> list[dict]:
    """The namespace's active observed facts (not earlier inferences)."""
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(
            """
            SELECT mi.id::text AS id, mv.content, mi.sensitivity::text AS sensitivity
            FROM memory_items mi
            JOIN memory_versions mv ON mv.id = mi.current_version_id
            WHERE mi.namespace_id = %s AND mi.status = 'active'
              AND COALESCE(mv.structured_content->>'inferred', 'false') <> 'true'
            ORDER BY mi.created_at DESC
            LIMIT %s
            """,
            (namespace_id, MAX_FACTS_PER_RUN),
        )
        return cur.fetchall()


def _load_existing_patterns(conn: psycopg.Connection, namespace_id) -> list[str]:
    """Patterns already stored as inferred facts or awaiting review."""
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(
            """
            SELECT mv.content
            FROM memory_items mi
            JOIN memory_versions mv ON mv.id = mi.current_version_id
            WHERE mi.namespace_id = %s AND mi.status = 'active'
              AND mv.structured_content->>'inferred' = 'true'
            UNION ALL
            SELECT content FROM memory_candidates
            WHERE namespace_id = %s AND status = 'pending_review'
              AND conflict_assessment->>'inferred' = 'true'
            """,
            (namespace_id, namespace_id),
        )
        return [r["content"] for r in cur.fetchall()]


_SENSITIVITY_ORDER = ["public", "internal", "confidential", "restricted", "secret"]


def pattern_sensitivity(sources: list[dict]) -> str:
    """The most sensitive class among a pattern's source facts."""
    level = "internal"
    for fact in sources:
        s = fact.get("sensitivity") or "internal"
        if s in _SENSITIVITY_ORDER and _SENSITIVITY_ORDER.index(s) > _SENSITIVITY_ORDER.index(level):
            level = s
    return level


def _propose_pattern(
    conn: psycopg.Connection, tenant_id, namespace_id, owner_principal_id,
    content: str, sources: list[dict], confidence: float,
) -> str:
    """Records a synthesis event and proposes the pattern for review as a
    memory_candidates row (pending_review). The candidate's
    conflict_assessment carries what approval needs to store it as an
    inferred fact: the cited source facts and the derivation."""
    candidate_id = str(uuid.uuid4())
    event_id = str(uuid.uuid4())
    source_ids = [f["id"] for f in sources]
    derivation = f"synthesis: pattern distilled from {len(source_ids)} source facts"
    confidence = max(0.0, min(1.0, float(confidence)))
    with conn.cursor() as cur:
        cur.execute(
            """
            INSERT INTO aggregate_sequences (aggregate_id, last_sequence) VALUES (%s, 1)
            ON CONFLICT (aggregate_id) DO UPDATE SET last_sequence = aggregate_sequences.last_sequence + 1, updated_at = now()
            RETURNING last_sequence
            """,
            (candidate_id,),
        )
        sequence = cur.fetchone()[0]
        cur.execute(
            """
            INSERT INTO events (id, tenant_id, aggregate_id, aggregate_sequence, source, event_type, subject,
                                occurred_at, data_schema, correlation_id, actor_id, data)
            VALUES (%s,%s,%s,%s,'scos.consolidation-worker','memory.synthesized',%s,now(),
                    'https://schemas.scos.local/events/event-envelope.schema.json',%s,%s,%s::jsonb)
            """,
            (event_id, tenant_id, candidate_id, sequence, "candidate/" + candidate_id, candidate_id,
             owner_principal_id, json.dumps({"namespace_id": str(namespace_id), "source_fact_ids": source_ids})),
        )
        cur.execute(
            """
            INSERT INTO memory_candidates
                (id, tenant_id, namespace_id, source_event_id, proposed_type, title, content,
                 authority_level, confidence_score, sensitivity, conflict_assessment, status)
            VALUES (%s,%s,%s,%s,'semantic',%s,%s,60,%s,%s,%s::jsonb,'pending_review')
            """,
            (candidate_id, tenant_id, namespace_id, event_id, content[:120], content, confidence,
             pattern_sensitivity(sources),
             json.dumps({"inferred": True, "derivation": derivation, "source_fact_ids": source_ids,
                         "evidence": [{"kind": "fact_citation", "reference_id": i} for i in source_ids]})),
        )
    return candidate_id


def _mark_synthesis_run(conn: psycopg.Connection, namespace_id, pattern_count: int) -> None:
    with conn.cursor() as cur:
        cur.execute(
            """
            INSERT INTO memory_synthesis_state (namespace_id, last_run_at, last_pattern_count, updated_at)
            VALUES (%s, now(), %s, now())
            ON CONFLICT (namespace_id) DO UPDATE
              SET last_run_at = now(), last_pattern_count = EXCLUDED.last_pattern_count, updated_at = now()
            """,
            (namespace_id, pattern_count),
        )


def run_synthesis_cycle(conn: psycopg.Connection) -> int:
    """Runs one pass over every namespace due for synthesis.

    Returns the number of patterns proposed for review. A no-op (returns 0)
    without DEEPSEEK_API_KEY configured -- see module docstring.
    """
    config = _deepseek_config()
    if config is None:
        return 0
    api_key, base_url, model = config

    written = 0
    for ns in _eligible_namespaces(conn):
        namespace_id = ns["namespace_id"]
        if not _due_for_synthesis(conn, namespace_id):
            conn.rollback()
            continue
        _act_as(conn, ns["tenant_id"], ns["acting_principal_id"])
        facts = _load_facts_for_synthesis(conn, namespace_id)
        pattern_count = 0
        try:
            known = _load_existing_patterns(conn, namespace_id)
            result = _call_deepseek(api_key, base_url, model, [{"id": f["id"], "content": f["content"]} for f in facts])
            by_id = {f["id"]: f for f in facts}
            for pattern in result.get("patterns", []):
                sources = [by_id[s] for s in pattern.get("source_fact_ids", []) if s in by_id]
                content = str(pattern.get("content", "")).strip()
                if len(sources) < MIN_SOURCE_FACTS_PER_PATTERN or not content:
                    continue
                if any(duplicate_score(content, k) >= DUPLICATE_OVERLAP for k in known):
                    continue
                _propose_pattern(
                    conn, ns["tenant_id"], namespace_id, ns["acting_principal_id"],
                    content, sources, pattern.get("confidence", 0.6),
                )
                known.append(content)
                pattern_count += 1
                written += 1
            conn.commit()
        except (urllib.error.URLError, TimeoutError, ValueError, KeyError,
                json.JSONDecodeError, psycopg.Error) as exc:
            logger.warning("synthesis for namespace %s failed: %s", namespace_id, exc)
            conn.rollback()
            continue
        _mark_synthesis_run(conn, namespace_id, pattern_count)
        conn.commit()
        if pattern_count:
            logger.info("synthesis: namespace %s produced %d pattern fact(s)", namespace_id, pattern_count)
    return written
