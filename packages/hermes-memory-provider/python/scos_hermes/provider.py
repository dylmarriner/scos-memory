"""Python Hermes memory provider package (§24)."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
import json
import os
from pathlib import Path
from typing import Optional


@dataclass
class MemoryEntry:
    id: str
    key: str
    value: str
    scope: str
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


@dataclass
class Turn:
    session_id: str
    turn_id: str
    messages: list = field(default_factory=list)
    tool_calls: list = field(default_factory=list)
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


class HermesMemoryProvider:
    def __init__(self, profile_id: str, agent_id: str, store_path: Optional[str] = None):
        self.profile_id = profile_id
        self.agent_id = agent_id
        self._memories: dict[str, MemoryEntry] = {}
        self._turns: list[Turn] = []
        self._store_path = store_path or os.getenv("SCOS_HERMES_MEMORY_STORE_PATH")
        self._load()

    @property
    def _profile_key(self) -> str:
        return f"{self.profile_id}/{self.agent_id}"

    def _load(self) -> None:
        if not self._store_path:
            return
        try:
            data = json.loads(Path(self._store_path).read_text())
            state = data.get(self._profile_key, {})
            self._memories = {
                key: MemoryEntry(key=value["key"], value=value["value"], scope=value["scope"],
                                 id=value["id"], created_at=datetime.fromisoformat(value["created_at"]))
                for key, value in state.get("memories", {}).items()
            }
            self._turns = [Turn(**{**turn, "created_at": datetime.fromisoformat(turn["created_at"])})
                           for turn in state.get("turns", [])]
        except (FileNotFoundError, ValueError, KeyError, json.JSONDecodeError):
            return

    def _persist(self) -> None:
        if not self._store_path:
            return
        path = Path(self._store_path)
        data = {}
        if path.exists():
            try:
                data = json.loads(path.read_text())
            except json.JSONDecodeError:
                data = {}
        data[self._profile_key] = {
            "memories": {key: {**vars(value), "created_at": value.created_at.isoformat()}
                         for key, value in self._memories.items()},
            "turns": [{**vars(turn), "created_at": turn.created_at.isoformat()} for turn in self._turns],
        }
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary = path.with_suffix(path.suffix + ".tmp")
        temporary.write_text(json.dumps(data, indent=2))
        temporary.replace(path)

    def write_memory(self, key: str, value: str, scope: str = "user") -> MemoryEntry:
        entry = MemoryEntry(
            id=f"hermes-py-{len(self._memories) + 1}",
            key=key,
            value=value,
            scope=scope,
        )
        self._memories[key] = entry
        self._persist()
        return entry

    def read_memory(self, key: str) -> Optional[MemoryEntry]:
        return self._memories.get(key)

    def sync_turn(self, session_id: str, turn_id: str, messages: list, tool_calls: Optional[list] = None) -> bool:
        if any(turn.session_id == session_id and turn.turn_id == turn_id for turn in self._turns):
            return False
        self._turns.append(Turn(session_id, turn_id, messages, tool_calls or []))
        self._persist()
        return True

    def recall(self, query: str = "", max_items: int = 20) -> list[Turn]:
        matches = [turn for turn in reversed(self._turns)
                   if not query or query in json.dumps([turn.messages, turn.tool_calls])]
        return matches[:max_items]
