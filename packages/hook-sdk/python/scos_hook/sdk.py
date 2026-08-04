"""Python hook SDK package (§22.6)."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Callable, Optional


@dataclass
class HookContext:
    session_id: str
    tenant_id: str
    stage: str
    payload: str
    attributes: dict[str, str] = field(default_factory=dict)


@dataclass
class HookResult:
    modified_payload: Optional[str] = None
    abort_execution: bool = False
    reason: str = ""


class HookRegistry:
    def __init__(self):
        self._hooks: dict[str, list[Callable[[HookContext], HookResult]]] = {}

    def register(self, stage: str, handler: Callable[[HookContext], HookResult]) -> None:
        self._hooks.setdefault(stage, []).append(handler)

    def dispatch(self, context: HookContext) -> HookResult:
        handlers = self._hooks.get(context.stage, [])
        current_payload = context.payload
        for handler in handlers:
            res = handler(context)
            if res.abort_execution:
                return res
            if res.modified_payload is not None:
                current_payload = res.modified_payload
                context.payload = current_payload
        return HookResult(modified_payload=current_payload)
