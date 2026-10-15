"""Python Hermes memory provider package (§24).

Implements the complete Hermes ``MemoryProvider`` lifecycle required by §24.2
on top of a profile-isolated local store. Automatic recall, persistence and
consolidation run through the lifecycle hooks, so continuity never depends on
the Hermes model choosing to call a memory tool (§24.3).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
import json
import os
from pathlib import Path
from typing import Any, Callable, Optional

# The provider name Hermes registers this plugin under (§24.1).
PROVIDER_NAME = "scos-nexus"

# Configuration keys the setup contract accepts (§24.4). Secrets are never
# among them: `optional_auth_reference` names a Hermes secret rather than
# carrying its value.
CONFIG_KEYS = (
    "base_url",
    "profile_scope",
    "tenant_id",
    "principal_id",
    "workspace",
    "transport_security",
    "optional_auth_reference",
)

# Execution contexts §24.6 requires be distinguished so automated and
# delegated work cannot corrupt the user representation.
CONTEXT_PRIMARY = "primary"
CONTEXT_SUBAGENT = "subagent"
CONTEXT_CRON = "cron"
CONTEXT_FLUSH = "flush"
VALID_CONTEXTS = (CONTEXT_PRIMARY, CONTEXT_SUBAGENT, CONTEXT_CRON, CONTEXT_FLUSH)


@dataclass
class MemoryEntry:
    id: str
    key: str
    value: str
    scope: str
    # Provenance travels with every entry so mirrored built-in writes and
    # delegated observations remain attributable (§24.3).
    source: str = "provider"
    context: str = CONTEXT_PRIMARY
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


@dataclass
class Turn:
    session_id: str
    turn_id: str
    messages: list = field(default_factory=list)
    tool_calls: list = field(default_factory=list)
    context: str = CONTEXT_PRIMARY
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


@dataclass
class PendingWork:
    """A queued unit of work retained until it is drained.

    Queued work is persisted rather than held only in memory so ``shutdown()``
    can flush it and anything undrained stays replayable (§24.3).
    """

    kind: str
    payload: dict
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


@dataclass
class FlushResult:
    """What a flush actually did.

    ``delivered`` counts items that were processed and removed from the queue.
    Everything else stays queued (and persisted), so it is replayable, and is
    listed in ``undelivered`` with the reason it could not be delivered.
    """

    delivered: int = 0
    undelivered: list = field(default_factory=list)

    @property
    def pending(self) -> int:
        return len(self.undelivered)


class ProviderUnavailable(RuntimeError):
    """Raised when an operation needs a backend that is not reachable."""


class HermesMemoryProvider:
    """SCOS Nexus as a first-class Hermes memory provider."""

    # Bounded queue: pre-warm and sync work is capped so an unreachable backend
    # cannot grow the queue without limit.
    MAX_QUEUE_DEPTH = 256

    def __init__(
        self,
        profile_id: str,
        agent_id: str,
        store_path: Optional[str] = None,
        config: Optional[dict] = None,
        recaller: Optional[Callable[[str, int], list]] = None,
        ingester: Optional[Callable[[Turn], None]] = None,
    ):
        self.profile_id = profile_id
        self.agent_id = agent_id
        self._memories: dict[str, MemoryEntry] = {}
        self._turns: list[Turn] = []
        self._queue: list[PendingWork] = []
        # Results of drained ``prefetch`` work, keyed by (query, max_items) and
        # consumed by the next matching ``prefetch`` call.
        self._prefetched: dict[tuple[str, int], list] = {}
        # Delivers a stored turn to the full-ingestion backend; raises on
        # failure. Without one, ``ingest_turn`` work stays queued.
        self._ingester = ingester
        self._lineage: list[dict] = []
        self._store_path = store_path or os.getenv("SCOS_HERMES_MEMORY_STORE_PATH")
        self._config = dict(config or {})
        self._recaller = recaller
        self._initialized = False
        self._session_id: Optional[str] = None
        self._load()

    # ---- Hermes lifecycle: identity and availability (§24.2) ----

    @property
    def name(self) -> str:
        return PROVIDER_NAME

    def is_available(self) -> bool:
        """Report readiness without performing network calls (§24.7).

        Availability is a local determination: the provider is usable once it
        has been initialized against a profile path.
        """
        return self._initialized and bool(self.profile_id and self.agent_id)

    def initialize(self, profile_path: Optional[str] = None, context: str = CONTEXT_PRIMARY) -> None:
        """Bind the provider to the active Hermes profile path (§24.6)."""
        if context not in VALID_CONTEXTS:
            raise ValueError(f"unknown execution context: {context}")
        if profile_path:
            self._store_path = str(Path(profile_path) / "scos-nexus-memory.json")
            self._load()
        self._context = context
        self._initialized = True

    def system_prompt_block(self) -> str:
        """Return the always-present memory preamble.

        The block states that recall is automatic precisely so the model is
        never required to call a tool for ordinary continuity (§24.3).
        """
        if not self._turns and not self._memories:
            return ""
        return (
            "SCOS Nexus memory is active. Relevant prior context is retrieved "
            "and persisted automatically; you do not need to call a memory tool."
        )

    # ---- Hermes lifecycle: recall and persistence (§24.3) ----

    def prefetch(self, query: str = "", max_items: int = 20) -> list[Turn]:
        """Return authorised cached recall before the model call.

        A result pre-warmed by draining ``queue_prefetch`` work is served once;
        otherwise recall runs now.
        """
        cached = self._prefetched.pop((query, max_items), None)
        if cached is not None:
            return cached
        return self._run_recall(query, max_items)

    def queue_prefetch(self, query: str, max_items: int = 20) -> bool:
        """Queue a pre-warm of likely next-turn context; ``flush`` runs it."""
        return self._enqueue("prefetch", {"query": query, "max_items": max_items})

    def _run_recall(self, query: str, max_items: int) -> list:
        if self._recaller is not None:
            return self._recaller(query, max_items)
        return self.recall(query, max_items)

    def sync_turn(
        self,
        session_id: str,
        turn_id: str,
        messages: list,
        tool_calls: Optional[list] = None,
        context: str = CONTEXT_PRIMARY,
    ) -> bool:
        """Record a turn exactly once, enqueuing full ingestion separately.

        The durable write happens inline so the turn cannot be lost; the
        heavier ingestion is queued so the user interface is never blocked.
        """
        if any(t.session_id == session_id and t.turn_id == turn_id for t in self._turns):
            return False
        self._turns.append(Turn(session_id, turn_id, messages, tool_calls or [], context))
        # Pre-warmed recall predates this turn and would omit it.
        self._prefetched.clear()
        self._enqueue("ingest_turn", {"session_id": session_id, "turn_id": turn_id})
        self._persist()
        return True

    def recall(self, query: str = "", max_items: int = 20) -> list[Turn]:
        matches = [
            turn
            for turn in reversed(self._turns)
            if not query or query in json.dumps([turn.messages, turn.tool_calls])
        ]
        return matches[:max_items]

    # ---- Hermes lifecycle: session and delegation hooks (§24.3) ----

    def on_turn_start(self, session_id: str, turn_id: str) -> list[Turn]:
        """Hydrate context at turn start without an explicit tool call."""
        self._session_id = session_id
        return self.prefetch("", max_items=10)

    def on_session_end(self, session_id: str) -> FlushResult:
        """Flush queued work, reporting anything that could not be delivered."""
        return self.flush()

    def on_session_switch(self, from_session: Optional[str], to_session: str, reason: str = "resume") -> dict:
        """Preserve lineage across resume, branch, reset and compression (§24.3)."""
        record = {
            "from_session": from_session,
            "to_session": to_session,
            "reason": reason,
            "recorded_at": datetime.now(timezone.utc).isoformat(),
        }
        self._lineage.append(record)
        self._session_id = to_session
        self._persist()
        return record

    def on_pre_compress(self, transcript: list) -> list[MemoryEntry]:
        """Preserve durable insights before the transcript is reduced.

        Compression is lossy by design, so anything worth keeping is written
        to durable memory first rather than summarised away.
        """
        preserved = []
        for index, item in enumerate(transcript):
            text = item if isinstance(item, str) else json.dumps(item)
            entry = self.write_memory(
                key=f"precompress:{self._session_id or 'session'}:{index}",
                value=text,
                scope="episodic",
                source="pre_compress",
            )
            preserved.append(entry)
        return preserved

    def on_memory_write(self, key: str, value: str, scope: str = "user") -> MemoryEntry:
        """Mirror an authorised built-in memory change, with provenance."""
        return self.write_memory(key=key, value=value, scope=scope, source="hermes_builtin")

    def on_delegation(self, parent_session: str, subagent_id: str, observation: str) -> MemoryEntry:
        """Capture parent-observed subagent work in the subagent context."""
        return self.write_memory(
            key=f"delegation:{parent_session}:{subagent_id}",
            value=observation,
            scope="episodic",
            source="delegation",
            context=CONTEXT_SUBAGENT,
        )

    def shutdown(self) -> dict:
        """Flush bounded queues and report replayable pending work (§24.3).

        Undelivered work stays in the persisted queue, so a replacement
        provider on the same profile replays it.
        """
        result = self.flush()
        self._initialized = False
        return {
            "drained": result.delivered,
            "pending": len(self._queue),
            "undelivered": list(result.undelivered),
        }

    # ---- Hermes lifecycle: tools (§24.5) ----

    def get_tool_schemas(self) -> list[dict]:
        """Return the supplemental inspection and correction tools.

        These are supplemental only: automatic recall, persistence and
        consolidation continue when no tool is ever called.
        """
        def schema(name: str, description: str, properties: dict, required: list) -> dict:
            return {
                "name": name,
                "description": description,
                "input_schema": {"type": "object", "properties": properties, "required": required},
            }

        return [
            schema("scos_recall", "Recall stored turns matching a query.",
                   {"query": {"type": "string"}, "max_items": {"type": "integer"}}, []),
            schema("scos_explain", "Explain why an item is held and where it came from.",
                   {"key": {"type": "string"}}, ["key"]),
            schema("scos_user_model", "Inspect the stored user representation.", {}, []),
            schema("scos_memory_graph", "Inspect memory entries and their relationships.", {}, []),
            schema("scos_correct", "Correct a stored entry, preserving the prior version.",
                   {"key": {"type": "string"}, "value": {"type": "string"}}, ["key", "value"]),
            schema("scos_forget", "Delete an eligible stored entry.",
                   {"key": {"type": "string"}}, ["key"]),
            schema("scos_status", "Report provider status and queue depth.", {}, []),
        ]

    def handle_tool_call(self, tool_name: str, arguments: Optional[dict] = None) -> Any:
        args = arguments or {}
        if tool_name == "scos_recall":
            return [vars(t) for t in self.recall(args.get("query", ""), args.get("max_items", 20))]
        if tool_name == "scos_explain":
            return self.explain(args["key"])
        if tool_name == "scos_user_model":
            return self.user_model()
        if tool_name == "scos_memory_graph":
            return self.memory_graph()
        if tool_name == "scos_correct":
            return vars(self.correct(args["key"], args["value"]))
        if tool_name == "scos_forget":
            return {"forgotten": self.forget(args["key"])}
        if tool_name == "scos_status":
            return self.status()
        raise ValueError(f"unknown tool: {tool_name}")

    # ---- Hermes lifecycle: configuration and backup (§24.4) ----

    def get_config_schema(self) -> dict:
        """Return the smallest necessary setup contract (§24.4)."""
        return {
            "type": "object",
            "properties": {
                "base_url": {"type": "string", "description": "SCOS Nexus base URL."},
                "profile_scope": {"type": "string", "description": "Hermes profile scope."},
                "tenant_id": {"type": "string"},
                "principal_id": {"type": "string"},
                "workspace": {"type": "string"},
                "transport_security": {"type": "string", "enum": ["tls", "mtls"]},
                "optional_auth_reference": {
                    "type": "string",
                    "description": "Name of a Hermes-managed secret. Never the secret value.",
                },
            },
            "required": ["base_url", "tenant_id", "principal_id"],
        }

    def save_config(self, config: dict) -> dict:
        """Validate and store configuration, refusing inline secrets."""
        unknown = set(config) - set(CONFIG_KEYS)
        if unknown:
            raise ValueError(f"unsupported configuration keys: {sorted(unknown)}")
        for required in ("base_url", "tenant_id", "principal_id"):
            if not str(config.get(required, "")).strip():
                raise ValueError(f"{required} is required")
        # §24.4: secrets stay in Hermes secret configuration, never provider
        # JSON. The auth field may only name a secret, not carry one.
        for banned in ("token", "password", "secret", "api_key"):
            if banned in config:
                raise ValueError(f"{banned} must be stored in Hermes secret configuration")
        self._config = dict(config)
        self._persist()
        return dict(self._config)

    def backup_paths(self) -> list[str]:
        """Return paths Hermes should include in profile backups."""
        return [self._store_path] if self._store_path else []

    # ---- Tool implementations ----

    def explain(self, key: str) -> dict:
        entry = self._memories.get(key)
        if entry is None:
            return {"key": key, "found": False}
        return {
            "key": key,
            "found": True,
            "value": entry.value,
            "scope": entry.scope,
            "source": entry.source,
            "context": entry.context,
            "created_at": entry.created_at.isoformat(),
        }

    def user_model(self) -> dict:
        """Return the stored user representation.

        Only primary-context entries contribute: §24.6 requires subagent, cron
        and flush work be kept out of the user representation.
        """
        return {
            "profile_id": self.profile_id,
            "agent_id": self.agent_id,
            "entries": [
                {"key": e.key, "value": e.value, "scope": e.scope, "source": e.source}
                for e in self._memories.values()
                if e.context == CONTEXT_PRIMARY
            ],
        }

    def memory_graph(self) -> dict:
        nodes = [{"id": e.key, "scope": e.scope, "source": e.source} for e in self._memories.values()]
        edges = [
            {"from": e.key, "to": e.source, "relation": "derived_from"}
            for e in self._memories.values()
            if e.source != "provider"
        ]
        return {"nodes": nodes, "edges": edges}

    def correct(self, key: str, value: str) -> MemoryEntry:
        """Correct an entry while preserving the superseded version.

        §4 forbids deleting history during correction, so the prior value is
        retained under a versioned key rather than overwritten.
        """
        existing = self._memories.get(key)
        if existing is not None:
            archive_key = f"{key}@{existing.created_at.isoformat()}"
            self._memories[archive_key] = MemoryEntry(
                id=f"{existing.id}-superseded",
                key=archive_key,
                value=existing.value,
                scope=existing.scope,
                source="superseded",
                context=existing.context,
            )
        return self.write_memory(key=key, value=value, scope="user", source="correction")

    def forget(self, key: str) -> bool:
        if key not in self._memories:
            return False
        del self._memories[key]
        self._persist()
        return True

    def status(self) -> dict:
        return {
            "provider": PROVIDER_NAME,
            "available": self.is_available(),
            "turns": len(self._turns),
            "memories": len(self._memories),
            "queue_depth": len(self._queue),
            "lineage_events": len(self._lineage),
        }

    # ---- Storage ----

    def write_memory(
        self,
        key: str,
        value: str,
        scope: str = "user",
        source: str = "provider",
        context: str = CONTEXT_PRIMARY,
    ) -> MemoryEntry:
        entry = MemoryEntry(
            id=f"hermes-py-{len(self._memories) + 1}",
            key=key,
            value=value,
            scope=scope,
            source=source,
            context=context,
        )
        self._memories[key] = entry
        self._persist()
        return entry

    def read_memory(self, key: str) -> Optional[MemoryEntry]:
        return self._memories.get(key)

    def lineage(self) -> list[dict]:
        return list(self._lineage)

    def pending_work(self) -> list[PendingWork]:
        return list(self._queue)

    def flush(self) -> FlushResult:
        """Process queued work, keeping anything that could not be delivered.

        ``prefetch`` work runs recall and holds the result for the next
        matching ``prefetch`` call. ``ingest_turn`` work hands the stored turn
        to the ingester. An item is removed only once it has been processed;
        a failed or undeliverable item stays queued and is reported.
        """
        result = FlushResult()
        remaining: list[PendingWork] = []
        for item in self._queue:
            error = self._deliver(item)
            if error is None:
                result.delivered += 1
                continue
            remaining.append(item)
            result.undelivered.append({"kind": item.kind, "payload": dict(item.payload), "error": error})
        self._queue = remaining
        self._persist()
        return result

    def _deliver(self, item: PendingWork) -> Optional[str]:
        """Process one queued item. Returns None on success, else the reason."""
        if item.kind == "prefetch":
            query = str(item.payload.get("query", ""))
            max_items = int(item.payload.get("max_items", 20))
            try:
                self._prefetched[(query, max_items)] = self._run_recall(query, max_items)
            except Exception as exc:  # recaller is caller-supplied
                return f"prefetch failed: {exc}"
            return None
        if item.kind == "ingest_turn":
            if self._ingester is None:
                return "no ingestion backend configured"
            session_id = item.payload.get("session_id")
            turn_id = item.payload.get("turn_id")
            turn = next(
                (t for t in self._turns if t.session_id == session_id and t.turn_id == turn_id),
                None,
            )
            if turn is None:
                return f"turn {session_id}/{turn_id} is not in the store"
            try:
                self._ingester(turn)
            except Exception as exc:  # ingester is caller-supplied
                return f"ingestion failed: {exc}"
            return None
        return f"unknown work kind: {item.kind}"

    def _enqueue(self, kind: str, payload: dict) -> bool:
        """Append bounded work. Returns False when the queue is saturated."""
        if len(self._queue) >= self.MAX_QUEUE_DEPTH:
            return False
        self._queue.append(PendingWork(kind=kind, payload=payload))
        self._persist()
        return True

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
                key: MemoryEntry(
                    key=value["key"],
                    value=value["value"],
                    scope=value["scope"],
                    id=value["id"],
                    source=value.get("source", "provider"),
                    context=value.get("context", CONTEXT_PRIMARY),
                    created_at=datetime.fromisoformat(value["created_at"]),
                )
                for key, value in state.get("memories", {}).items()
            }
            self._turns = [
                Turn(**{**turn, "created_at": datetime.fromisoformat(turn["created_at"])})
                for turn in state.get("turns", [])
            ]
            self._queue = [
                PendingWork(
                    kind=item["kind"],
                    payload=item["payload"],
                    created_at=datetime.fromisoformat(item["created_at"]),
                )
                for item in state.get("queue", [])
            ]
            self._lineage = state.get("lineage", [])
            self._config = state.get("config", self._config)
        except (FileNotFoundError, ValueError, KeyError, TypeError, json.JSONDecodeError):
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
            "memories": {
                key: {**vars(value), "created_at": value.created_at.isoformat()}
                for key, value in self._memories.items()
            },
            "turns": [{**vars(turn), "created_at": turn.created_at.isoformat()} for turn in self._turns],
            "queue": [{**vars(item), "created_at": item.created_at.isoformat()} for item in self._queue],
            "lineage": self._lineage,
            "config": self._config,
        }
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary = path.with_suffix(path.suffix + ".tmp")
        temporary.write_text(json.dumps(data, indent=2))
        temporary.replace(path)
