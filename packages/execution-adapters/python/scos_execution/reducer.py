"""Python execution proxy output reducer and command sanitizer (§20.6).

Command risk is graded by the same policy file the Go package embeds
(``packages/execution-adapters/risk_policy.json``), so both languages
classify every command identically.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path

RISK_READ = "read"
RISK_WRITE = "write"
RISK_TEST = "test"
RISK_DESTRUCTIVE = "destructive"

POLICY_PATH = Path(__file__).resolve().parents[2] / "risk_policy.json"


@dataclass
class CommandRewrite:
    original_command: str
    rewritten_command: str
    is_rewritten: bool
    reason: str = ""


@lru_cache(maxsize=1)
def _policy() -> dict:
    policy = json.loads(POLICY_PATH.read_text(encoding="utf-8"))
    if not policy.get("destructive_prefixes") or not policy.get("composition_characters"):
        raise RuntimeError(f"{POLICY_PATH} is missing destructive prefixes or composition characters")
    return policy


def classify_risk(command: str) -> str:
    """Grade a shell command as read, write, test or destructive."""
    lower = command.strip().lower()
    policy = _policy()
    destructive = policy["destructive_prefixes"]
    if any(lower.startswith(prefix) for prefix in destructive):
        return RISK_DESTRUCTIVE
    if any(ch in lower for ch in policy["composition_characters"]):
        if any(prefix.strip() in lower for prefix in destructive):
            return RISK_DESTRUCTIVE
    if any(lower.startswith(prefix) for prefix in policy["write_prefixes"]):
        return RISK_WRITE
    if any(lower.startswith(prefix) for prefix in policy["test_prefixes"]):
        return RISK_TEST
    return RISK_READ


class OutputReducer:
    def rewrite_command(self, command: str) -> CommandRewrite:
        """Block destructive commands; pass every other command through.

        Destructive commands must go through the execution-proxy service,
        which requires explicit confirmation and governance authorisation.
        """
        if not command.strip():
            raise ValueError("command is empty")
        if classify_risk(command) == RISK_DESTRUCTIVE:
            return CommandRewrite(
                original_command=command,
                rewritten_command="",
                is_rewritten=True,
                reason="destructive command blocked: route it through the execution proxy with explicit confirmation",
            )
        return CommandRewrite(original_command=command, rewritten_command=command, is_rewritten=False)

    def reduce_output(self, output: str, max_bytes: int = 2048) -> str:
        if max_bytes <= 0 or len(output) <= max_bytes:
            return output
        half = max_bytes // 2
        return output[:half] + "\n... [output reduced by execution-proxy] ...\n" + output[-half:]
