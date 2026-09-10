"""SCOS Nexus Memory Fabric — first-class Hermes MemoryProvider.

This plugin participates in Hermes' automatic memory lifecycle. Recall is
served through the SCOS MCP gateway, while durable lifecycle writes are sent
to the SCOS hermes-provider service. It deliberately uses only Hermes'
documented ``agent.memory_provider.MemoryProvider`` contract.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import queue
import ssl
import tempfile
import threading
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Any, Dict, List, Optional

from agent.memory_provider import MemoryProvider

logger = logging.getLogger(__name__)

_DEFAULT_GATEWAY_URL = ""
_CONFIG_FILENAME = "scos.json"
_PREFETCH_CHAR_LIMIT = 4000
_PERSISTENCE_QUEUE_LIMIT = 256
_SHUTDOWN_DRAIN_SECONDS = 5.0

FALLBACK_TOOLS = [
    {
        "name": "scos_recall",
        "description": "Search SCOS Nexus memory for relevant context.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {"type": "string"},
                "max_items": {"type": "integer"},
            },
            "required": ["query"],
        },
    },
    {
        "name": "scos_status",
        "description": "Check SCOS Nexus memory provider status.",
        "inputSchema": {"type": "object", "properties": {}, "required": []},
    },
]


class ScosMemoryProvider(MemoryProvider):
    """SCOS Nexus Memory Fabric as a Hermes memory provider."""

    pre_compress_checkpoint_api_version = 2

    def __init__(self):
        self._env: Dict[str, str] = {}
        self._hermes_home: Optional[Path] = None
        self._session_id = ""
        self._write_enabled = True
        self._persistence_warning_emitted = False
        self._write_queue: queue.Queue = queue.Queue(maxsize=_PERSISTENCE_QUEUE_LIMIT)
        self._worker: Optional[threading.Thread] = None
        self._worker_lock = threading.Lock()
        self._closed = False

    @property
    def name(self) -> str:
        return "scos"

    # -- config -------------------------------------------------------------

    def _env_path(self) -> Path:
        home = self._hermes_home or Path.home() / ".hermes"
        return Path(home) / "scos.env"

    def _config_path(self) -> Path:
        home = self._hermes_home or Path.home() / ".hermes"
        return Path(home) / _CONFIG_FILENAME

    def _load_env(self) -> Dict[str, str]:
        values = {
            "SCOS_GATEWAY_URL": _DEFAULT_GATEWAY_URL,
            "SCOS_TLS_CA_PATH": "",
        }
        config_path = self._config_path()
        if config_path.exists():
            try:
                config = json.loads(config_path.read_text())
                if isinstance(config, dict):
                    values.update(
                        {str(k): str(v) for k, v in config.items() if v is not None}
                    )
            except (OSError, json.JSONDecodeError) as error:
                logger.warning("unable to read %s: %s", config_path, error)

        env_path = self._env_path()
        if env_path.exists():
            for line in env_path.read_text().splitlines():
                line = line.strip()
                if line and "=" in line and not line.startswith("#"):
                    key, value = line.split("=", 1)
                    values[key] = value

        values.update({k: v for k, v in os.environ.items() if k.startswith("SCOS_")})
        return values

    def is_available(self) -> bool:
        env = self._load_env() if not self._env else self._env
        token = env.get("SCOS_BEARER_TOKEN") or env.get("SCOS_HERMES_BEARER_TOKEN")
        return bool(
            env.get("SCOS_GATEWAY_URL")
            and token
            and env.get("SCOS_TENANT_ID")
            and env.get("SCOS_PRINCIPAL_ID")
            and env.get("SCOS_HERMES_PROVIDER_URL")
            and env.get("SCOS_NAMESPACE_ID")
        )

    def unavailable_reason(self) -> str:
        return (
            "SCOS Nexus not configured: set SCOS_GATEWAY_URL, SCOS_BEARER_TOKEN, "
            "SCOS_TENANT_ID, SCOS_PRINCIPAL_ID, SCOS_HERMES_PROVIDER_URL and "
            f"SCOS_NAMESPACE_ID in {self._env_path()} or Hermes memory setup."
        )

    def get_config_schema(self) -> List[Dict[str, Any]]:
        return [
            {
                "key": "SCOS_GATEWAY_URL",
                "description": "SCOS gateway URL for recall and memory tools",
                "required": True,
            },
            {
                "key": "SCOS_BEARER_TOKEN",
                "description": "Bearer token for SCOS services",
                "secret": True,
                "required": True,
                "env_var": "SCOS_BEARER_TOKEN",
            },
            {"key": "SCOS_TENANT_ID", "description": "SCOS tenant ID", "required": True},
            {
                "key": "SCOS_PRINCIPAL_ID",
                "description": "SCOS principal ID",
                "required": True,
            },
            {
                "key": "SCOS_HERMES_PROVIDER_URL",
                "description": "SCOS Hermes memory lifecycle service URL",
                "required": True,
            },
            {
                "key": "SCOS_NAMESPACE_ID",
                "description": "Namespace for persisted Hermes memory",
                "required": True,
            },
        ]

    def save_config(self, values: Dict[str, Any], hermes_home: str) -> None:
        """Persist only non-secret setup values inside the active Hermes profile."""
        required = (
            "SCOS_GATEWAY_URL",
            "SCOS_TENANT_ID",
            "SCOS_PRINCIPAL_ID",
            "SCOS_HERMES_PROVIDER_URL",
            "SCOS_NAMESPACE_ID",
        )
        missing = [key for key in required if not str(values.get(key, "")).strip()]
        if missing:
            raise ValueError("SCOS setup is incomplete; missing: " + ", ".join(missing))

        allowed = set(required) | {"SCOS_TLS_CA_PATH"}
        unknown = sorted(
            set(values)
            - allowed
            - {"SCOS_BEARER_TOKEN", "SCOS_HERMES_BEARER_TOKEN"}
        )
        if unknown:
            raise ValueError("unknown SCOS setup fields: " + ", ".join(unknown))

        target = Path(hermes_home) / _CONFIG_FILENAME
        target.parent.mkdir(parents=True, exist_ok=True)
        payload = {
            key: str(values[key]).strip()
            for key in allowed
            if values.get(key) is not None and str(values[key]).strip()
        }
        fd, temporary = tempfile.mkstemp(
            prefix=f".{_CONFIG_FILENAME}.", dir=str(target.parent)
        )
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as stream:
                json.dump(payload, stream, indent=2, sort_keys=True)
                stream.write("\n")
                stream.flush()
                os.fchmod(stream.fileno(), 0o600)
            os.replace(temporary, target)
        except Exception:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass
            raise

    def post_setup(self, hermes_home: str, config: dict) -> None:
        """Verify recall and lifecycle surfaces before setup reports success."""
        del config
        self._hermes_home = Path(hermes_home)
        self._env = self._load_env()

        gateway = self._gateway_rpc("tools/list", {})
        if "error" in gateway:
            raise RuntimeError(
                f"SCOS recall check failed: {gateway['error'].get('message', gateway['error'])}"
            )
        tools = (gateway.get("result") or {}).get("tools")
        if not isinstance(tools, list) or not any(
            item.get("name") == "scos_recall" for item in tools if isinstance(item, dict)
        ):
            raise RuntimeError(
                "SCOS recall check failed: gateway did not advertise scos_recall"
            )

        provider_url = (
            self._env.get("SCOS_HERMES_PROVIDER_URL", "").rstrip("/")
            + "/v1/provider/status"
        )
        request = urllib.request.Request(
            provider_url, headers=self._service_headers(), method="GET"
        )
        try:
            with urllib.request.urlopen(
                request, timeout=8, context=self._ssl_context()
            ) as response:
                status = json.loads(response.read().decode() or "{}")
        except Exception as error:
            raise RuntimeError(f"SCOS persistence check failed: {error}") from error
        if not status.get("healthy", status.get("is_available", False)):
            raise RuntimeError(
                "SCOS persistence check failed: hermes-provider is not healthy"
            )

    # -- lifecycle ----------------------------------------------------------

    def initialize(self, session_id: str, **kwargs) -> None:
        hermes_home = kwargs.get("hermes_home")
        self._hermes_home = Path(hermes_home) if hermes_home else None
        self._session_id = session_id
        self._write_enabled = kwargs.get("agent_context", "primary") == "primary"
        self._env = self._load_env()
        self._closed = False

    def system_prompt_block(self) -> str:
        if not self.is_available():
            return ""
        return (
            "# SCOS Nexus Memory\n"
            "Active. Persistent cross-session memory is handled automatically by the "
            "SCOS Memory Fabric. Relevant recall is injected before turns. Use "
            "scos_recall only when automatic recall misses needed context.\n"
        )

    def queue_prefetch(self, query: str, *, session_id: str = "") -> None:
        """Ask the lifecycle service to pre-warm the current Hermes session."""
        if not query or not self._persistence_configured():
            return
        payload = self._session_payload(session_id or self._session_id or "unknown")
        payload.update({"query": query[:500], "max_items": 10})
        try:
            self._provider_post("/v1/provider/queue_prefetch", payload)
        except Exception as error:
            logger.debug("SCOS queue_prefetch failed: %s", error)

    def on_pre_compress(
        self,
        messages: List[Dict[str, Any]],
        *,
        require_checkpoint: bool = False,
    ) -> str:
        """Durably checkpoint normalized evidence before Hermes rewrites context.

        Hermes v2 hands checkpoint providers direct evidence only. SCOS archives that
        evidence synchronously and uses a content-derived turn id so retries are safe
        against the existing idempotent sync_turn endpoint.
        """
        if not messages:
            return ""
        if not self._write_enabled:
            if require_checkpoint:
                raise RuntimeError(
                    "SCOS durable checkpoint is disabled for non-primary Hermes agent context"
                )
            return ""
        if not self._persistence_configured():
            error = RuntimeError("SCOS durable checkpoint is not configured")
            if require_checkpoint:
                raise error
            logger.debug("SCOS pre-compress checkpoint skipped: %s", error)
            return ""

        sid = self._session_id or "unknown"
        session_uuid = self._session_uuid(sid)
        canonical = json.dumps(
            messages,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        )
        digest = hashlib.sha256(
            (
                session_uuid
                + "\0"
                + self._env.get("SCOS_NAMESPACE_ID", "")
                + "\0"
                + canonical
            ).encode("utf-8")
        ).hexdigest()
        payload = self._turn_payload(
            sid,
            "hermes-v2-checkpoint:" + digest,
            messages,
        )
        try:
            self._provider_post("/v1/provider/sync_turn", payload)
        except Exception as error:
            if require_checkpoint:
                raise
            logger.debug("SCOS best-effort pre-compress checkpoint failed: %s", error)
        return ""

    def on_memory_write(
        self,
        action: str,
        target: str,
        content: str,
        metadata: Optional[Dict[str, Any]] = None,
    ) -> None:
        """Mirror committed built-in MEMORY/USER state transitions into SCOS."""
        if not self._write_enabled or not self._persistence_configured():
            return
        if action not in {"add", "replace", "remove"} or target not in {"memory", "user"}:
            logger.debug("ignoring unsupported Hermes memory transition %s/%s", action, target)
            return

        metadata = dict(metadata or {})
        old_text = str(metadata.get("old_text") or "")
        key_material = old_text if action in {"replace", "remove"} and old_text else content
        content_hash = hashlib.sha256(key_material.encode("utf-8") + b"\0").hexdigest()
        payload = {
            "namespace_id": self._env.get("SCOS_NAMESPACE_ID", ""),
            "key": f"hermes:{target}:{content_hash}",
            "value": content,
            "scope": "user" if target == "user" else "working",
            "source": f"hermes_memory:{action}",
            "profile_path": str(self._hermes_home or ""),
        }
        self._enqueue_provider_post("/v1/provider/memory_write", payload)

    def on_session_end(self, messages: List[Dict[str, Any]]) -> None:
        """Drain turn writes, take one final durable checkpoint, then close the session."""
        if not self._write_enabled or not self._persistence_configured():
            return

        if not self._wait_for_pending(_SHUTDOWN_DRAIN_SECONDS):
            logger.warning(
                "SCOS session-end persistence queue did not fully drain before final checkpoint"
            )
        if messages:
            self.on_pre_compress(messages, require_checkpoint=False)
        try:
            self._provider_post(
                "/v1/provider/session_end",
                self._session_payload(self._session_id or "unknown"),
            )
        except Exception as error:
            logger.debug("SCOS session_end failed: %s", error)

    def shutdown(self) -> None:
        """Give queued writes a bounded drain, then stop the provider worker."""
        self._closed = True
        self._wait_for_pending(_SHUTDOWN_DRAIN_SECONDS)
        worker = self._worker
        if not worker or not worker.is_alive():
            return
        try:
            self._write_queue.put(("stop", None, None), timeout=0.25)
        except queue.Full:
            logger.warning("SCOS persistence worker queue remained saturated at shutdown")
            return
        worker.join(timeout=1.0)
        if worker.is_alive():
            logger.warning("SCOS persistence worker did not stop cleanly")

    # -- recall -------------------------------------------------------------

    def _ssl_context(self) -> ssl.SSLContext:
        ca_path = self._env.get("SCOS_TLS_CA_PATH", "")
        if ca_path and not Path(ca_path).exists():
            raise RuntimeError(f"SCOS_TLS_CA_PATH does not exist: {ca_path}")
        context = ssl.create_default_context(cafile=ca_path or None)
        if self._env.get("SCOS_TLS_INSECURE_SKIP_VERIFY") == "1":
            context.check_hostname = False
            context.verify_mode = ssl.CERT_NONE
        return context

    def _service_headers(self) -> Dict[str, str]:
        token = self._env.get("SCOS_BEARER_TOKEN") or self._env.get(
            "SCOS_HERMES_BEARER_TOKEN", ""
        )
        if (
            not token
            or not self._env.get("SCOS_TENANT_ID")
            or not self._env.get("SCOS_PRINCIPAL_ID")
        ):
            raise RuntimeError(
                "SCOS_BEARER_TOKEN, SCOS_TENANT_ID and SCOS_PRINCIPAL_ID are required"
            )
        return {
            "Content-Type": "application/json",
            "X-SCOS-Tenant-ID": self._env.get("SCOS_TENANT_ID", ""),
            "X-SCOS-Principal-ID": self._env.get("SCOS_PRINCIPAL_ID", ""),
            "Authorization": "Bearer " + token,
        }

    def _gateway_rpc(self, method: str, params: dict, timeout: float = 8) -> dict:
        url = self._env.get("SCOS_GATEWAY_URL")
        if not url:
            return {
                "error": {"code": -1, "message": "SCOS_GATEWAY_URL not configured"}
            }
        payload = {
            "jsonrpc": "2.0",
            "id": 1,
            "method": method,
            "params": params,
        }
        request = urllib.request.Request(
            url,
            data=json.dumps(payload).encode(),
            headers=self._service_headers(),
            method="POST",
        )
        try:
            with urllib.request.urlopen(
                request, timeout=timeout, context=self._ssl_context()
            ) as response:
                body = response.read().decode()
                return json.loads(body) if body else {}
        except urllib.error.HTTPError as error:
            return {
                "error": {
                    "code": error.code,
                    "message": error.read().decode(errors="replace")[:500],
                }
            }
        except Exception as error:
            return {"error": {"code": -1, "message": str(error)}}

    def prefetch(self, query: str, *, session_id: str = "") -> str:
        del session_id
        if not query or not self.is_available():
            return ""
        response = self._gateway_rpc(
            "tools/call",
            {
                "name": "scos_recall",
                "arguments": {"query": query[:500], "max_items": 10},
            },
        )
        if "error" in response:
            logger.debug("SCOS prefetch failed: %s", response["error"])
            return ""
        content = (response.get("result") or {}).get("content") or []
        text = ""
        for block in content:
            if block.get("type") == "text":
                text = block.get("text", "")
                break
        if not text.strip():
            return ""
        return f"## SCOS Nexus Memory\n{text[:_PREFETCH_CHAR_LIMIT]}"

    # -- durable lifecycle transport ---------------------------------------

    def _persistence_configured(self) -> bool:
        token = self._env.get("SCOS_HERMES_BEARER_TOKEN") or self._env.get(
            "SCOS_BEARER_TOKEN"
        )
        return bool(
            self._env.get("SCOS_HERMES_PROVIDER_URL")
            and token
            and self._env.get("SCOS_TENANT_ID")
            and self._env.get("SCOS_PRINCIPAL_ID")
            and self._env.get("SCOS_NAMESPACE_ID")
        )

    def _session_uuid(self, session_id: str) -> str:
        return _stable_session_uuid(
            session_id,
            self._env.get("SCOS_TENANT_ID", ""),
            self._env.get("SCOS_PRINCIPAL_ID", ""),
        )

    def _session_payload(self, session_id: str) -> Dict[str, Any]:
        return {
            "namespace_id": self._env.get("SCOS_NAMESPACE_ID", ""),
            "session_id": self._session_uuid(session_id),
            "tenant_id": self._env.get("SCOS_TENANT_ID", ""),
            "principal_id": self._env.get("SCOS_PRINCIPAL_ID", ""),
        }

    def _turn_payload(
        self,
        session_id: str,
        turn_id: str,
        messages: List[Dict[str, Any]],
    ) -> Dict[str, Any]:
        payload = self._session_payload(session_id)
        payload.update(
            {
                "turn_id": turn_id,
                "messages": messages,
                "tool_calls": [],
                "profile_path": str(self._hermes_home or ""),
            }
        )
        return payload

    def _provider_post(
        self, path: str, payload: Dict[str, Any], timeout: float = 8
    ) -> Dict[str, Any]:
        base_url = self._env.get("SCOS_HERMES_PROVIDER_URL", "").rstrip("/")
        if not base_url:
            raise RuntimeError("SCOS_HERMES_PROVIDER_URL not configured")
        request = urllib.request.Request(
            base_url + path,
            data=json.dumps(payload).encode(),
            headers=self._service_headers(),
            method="POST",
        )
        try:
            with urllib.request.urlopen(
                request, timeout=timeout, context=self._ssl_context()
            ) as response:
                body = response.read().decode()
                if response.status // 100 != 2:
                    raise RuntimeError(f"unexpected SCOS status {response.status}")
                return json.loads(body) if body else {}
        except urllib.error.HTTPError as error:
            detail = error.read().decode(errors="replace")[:500]
            raise RuntimeError(
                f"SCOS lifecycle request {path} failed with HTTP {error.code}: {detail}"
            ) from error
        except Exception as error:
            if isinstance(error, RuntimeError):
                raise
            raise RuntimeError(f"SCOS lifecycle request {path} failed: {error}") from error

    def _ensure_worker(self) -> bool:
        if self._closed:
            return False
        with self._worker_lock:
            if self._worker and self._worker.is_alive():
                return True
            self._worker = threading.Thread(
                target=self._worker_loop,
                daemon=True,
                name="scos-persistence",
            )
            self._worker.start()
            return True

    def _worker_loop(self) -> None:
        while True:
            kind, path, payload = self._write_queue.get()
            try:
                if kind == "stop":
                    return
                if kind == "barrier":
                    payload.set()
                    continue
                try:
                    self._provider_post(path, payload)
                except Exception as error:
                    logger.debug("SCOS background persistence failed: %s", error)
            finally:
                self._write_queue.task_done()

    def _enqueue_provider_post(self, path: str, payload: Dict[str, Any]) -> bool:
        if not self._ensure_worker():
            return False
        try:
            self._write_queue.put_nowait(("post", path, payload))
            return True
        except queue.Full:
            logger.error(
                "SCOS persistence queue saturated; final checkpoint will be relied on for recovery"
            )
            return False

    def _wait_for_pending(self, timeout: float) -> bool:
        worker = self._worker
        if not worker or not worker.is_alive():
            return True
        barrier = threading.Event()
        try:
            self._write_queue.put(("barrier", None, barrier), timeout=min(timeout, 0.25))
        except queue.Full:
            return False
        return barrier.wait(timeout)

    def sync_turn(
        self,
        user_content: str,
        assistant_content: str,
        *,
        session_id: str = "",
        messages: Optional[List[Dict[str, Any]]] = None,
    ) -> None:
        """Queue one completed exchange and return immediately.

        ``messages`` is intentionally not re-persisted here because Hermes supplies the
        entire transcript-so-far. Persisting it on every turn duplicates old messages;
        the v2 pre-compress checkpoint is the explicit full-evidence archival boundary.
        """
        del messages
        if not self._write_enabled or not user_content or not assistant_content:
            return
        if not self._persistence_configured():
            if not self._persistence_warning_emitted:
                logger.warning(
                    "SCOS turn persistence is not configured; set "
                    "SCOS_HERMES_PROVIDER_URL, SCOS_NAMESPACE_ID and a bearer token"
                )
                self._persistence_warning_emitted = True
            return

        sid = session_id or self._session_id or "unknown"
        turn_messages = [
            {"role": "user", "content": user_content},
            {"role": "assistant", "content": assistant_content},
        ]
        canonical = json.dumps(
            turn_messages,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        )
        turn_id = "hermes-turn:" + hashlib.sha256(
            (self._session_uuid(sid) + "\0" + canonical).encode("utf-8")
        ).hexdigest()
        self._enqueue_provider_post(
            "/v1/provider/sync_turn",
            self._turn_payload(sid, turn_id, turn_messages),
        )

    # -- explicit tools -----------------------------------------------------

    def _discover_tools(self) -> List[dict]:
        response = self._gateway_rpc("tools/list", {}, timeout=5)
        tools = (response.get("result") or {}).get("tools")
        return tools if isinstance(tools, list) and tools else FALLBACK_TOOLS

    def get_tool_schemas(self) -> List[Dict[str, Any]]:
        schemas = []
        for tool in self._discover_tools():
            name = tool.get("name")
            if not name:
                continue
            schemas.append(
                {
                    "name": name,
                    "description": tool.get("description", ""),
                    "parameters": tool.get("inputSchema")
                    or {"type": "object", "properties": {}, "required": []},
                }
            )
        return schemas

    def handle_tool_call(
        self, tool_name: str, args: Dict[str, Any], **kwargs
    ) -> str:
        del kwargs
        response = self._gateway_rpc(
            "tools/call", {"name": tool_name, "arguments": args or {}}
        )
        if "error" in response:
            return json.dumps({"error": response["error"]})
        result = response.get("result", {})
        content = result.get("content") or []
        for block in content:
            if block.get("type") == "text":
                try:
                    return json.dumps(json.loads(block["text"]))
                except (KeyError, json.JSONDecodeError):
                    return json.dumps({"text": block.get("text")})
        return json.dumps(result)


def _stable_session_uuid(session_id: str, tenant_id: str, principal_id: str) -> str:
    """Map Hermes' arbitrary session identifier to the SCOS UUID contract."""
    try:
        return str(uuid.UUID(session_id))
    except (ValueError, AttributeError, TypeError):
        identity = f"{tenant_id}\0{principal_id}\0{session_id}"
        return str(uuid.uuid5(uuid.NAMESPACE_URL, "scos-hermes-session:" + identity))


def register(ctx) -> None:
    """Register SCOS Nexus as the active external Hermes memory provider."""
    ctx.register_memory_provider(ScosMemoryProvider())
