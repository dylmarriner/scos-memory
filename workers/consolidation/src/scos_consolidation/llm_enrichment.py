"""Optional LLM-backed refinement of extracted memory candidates.

extractor.py's deterministic classification (memory_class, authority_level,
confidence_score, sensitivity_class) stays deterministic and untouched here
-- those numbers drive governance-critical promotion/review decisions
(promoter.py, review.py) and must remain auditable and reproducible without
depending on an external model's discretion.

What an LLM genuinely helps with is turning a raw conversation excerpt
("[user]\\nDoes X support Y?\\n\\n[assistant]\\nYes, X supports Y via Z.")
into the kind of concise, standalone factual statement mem0's single-pass
extraction produces ("X supports Y via Z."). That is the one thing this
module does: rewrite title/content for clarity. It never changes
memory_class, authority, confidence, sensitivity, or evidence.

Disabled by default (no DEEPSEEK_API_KEY configured) and fails silently
closed on any error -- a network hiccup or a malformed model response must
never break candidate extraction, so every failure mode here falls back to
returning the original, deterministically-extracted candidates unchanged.
"""

from __future__ import annotations

import json
import logging
import os
import urllib.error
import urllib.request
from typing import Any

from pydantic import ValidationError

from .models import MemoryCandidate

logger = logging.getLogger(__name__)

_SYSTEM_PROMPT = (
    "You rewrite a raw conversation excerpt into a single, concise, "
    "standalone factual statement suitable for long-term memory storage. "
    "Do not add information that is not present in the excerpt. Do not "
    "include conversational framing (\"the user said\", \"I responded\") "
    "-- state the fact directly. Respond with a JSON object of the exact "
    "shape {\"title\": \"short title, at most 200 characters\", "
    "\"content\": \"the rewritten statement\"}."
)


def _deepseek_config() -> tuple[str, str, str] | None:
    api_key = os.environ.get("DEEPSEEK_API_KEY", "").strip()
    if not api_key:
        return None
    base_url = os.environ.get("DEEPSEEK_BASE_URL", "").strip().rstrip("/") or "https://api.deepseek.com/v1"
    model = os.environ.get("DEEPSEEK_MODEL", "").strip() or "deepseek-chat"
    return api_key, base_url, model


def _call_deepseek(api_key: str, base_url: str, model: str, user_content: str, timeout: float = 15.0) -> dict[str, Any]:
    body = json.dumps({
        "model": model,
        "messages": [
            {"role": "system", "content": _SYSTEM_PROMPT},
            {"role": "user", "content": user_content},
        ],
        "temperature": 0.1,
        "max_tokens": 512,
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
    return json.loads(content)


def enrich_candidates(candidates: list[MemoryCandidate]) -> list[MemoryCandidate]:
    """Best-effort LLM rewrite of each candidate's title/content.

    Returns candidates unchanged (same list, same objects) when no API key
    is configured. On a per-candidate failure, that one candidate is kept
    as extracted and the rest of the batch still gets a chance to enrich.
    """
    config = _deepseek_config()
    if config is None:
        return candidates
    api_key, base_url, model = config

    enriched: list[MemoryCandidate] = []
    for candidate in candidates:
        try:
            result = _call_deepseek(api_key, base_url, model, candidate.content)
            new_title = str(result.get("title", "")).strip()[:240]
            new_content = str(result.get("content", "")).strip()
            if not new_title or not new_content:
                raise ValueError("model returned an empty title or content")
            metadata = {**candidate.metadata, "rewritten_by": model, "extracted_content": candidate.content}
            enriched.append(candidate.model_copy(update={"title": new_title, "content": new_content, "metadata": metadata}))
        except (urllib.error.URLError, TimeoutError, ValueError, KeyError, IndexError,
                json.JSONDecodeError, ValidationError) as exc:
            logger.warning("candidate %s: LLM enrichment skipped (%s)", candidate.candidate_id, exc)
            enriched.append(candidate)
    return enriched
