"""Python execution proxy output reducer and command sanitizer (§20.6)."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass
class CommandRewrite:
    original_command: str
    rewritten_command: str
    is_rewritten: bool
    reason: str = ""


class OutputReducer:
    def rewrite_command(self, command: str) -> CommandRewrite:
        trimmed = command.strip()
        if "rm -rf /" in trimmed:
            return CommandRewrite(
                original_command=command,
                rewritten_command="echo 'blocked unsafe recursive root delete command'",
                is_rewritten=True,
                reason="blocked unsafe command",
            )
        return CommandRewrite(
            original_command=command,
            rewritten_command=command,
            is_rewritten=False,
        )

    def reduce_output(self, output: str, max_bytes: int = 2048) -> str:
        if max_bytes <= 0 or len(output) <= max_bytes:
            return output
        half = max_bytes // 2
        return output[:half] + "\n... [output reduced by execution-proxy] ...\n" + output[-half:]
