"""SCOS Nexus Memory Fabric — MemoryProvider implementation for Hermes Agent.

Unlike the generic ``plugins/scos-memory`` toolset plugin (tool registration
only), this is a first-class Hermes ``MemoryProvider``: it participates in
the same prefetch-before-turn / sync-after-turn lifecycle as the bundled
mem0/honcho/hindsight providers, so recall and persistence happen without
the model having to choose to call a tool — mirroring what the Claude Code
and Codex integrations already do via their scos-auto-recall / scos-auto-persist
hooks (GOVERNANCE.md 4.14/21.4 - platform-owned retrieval and persistence).

Two SCOS surfaces are used, matching the other agent integrations exactly:
  - mcp-gateway (tools/list, tools/call)     — recall + generic tool access
  - SCOS hermes-provider /v1/provider/sync_turn — complete turn persistence

The SCOS hermes-provider URL below is a memory lifecycle service URL. It is
not Hermes' inference/model-provider URL: this module deliberately never reads
OPENAI_*, OPENROUTER_*, or any other model-provider setting.

Reads the same ``~/.hermes/scos.env`` connection file the other SCOS
integrations (and the scos-mcp-bridge) share, so setting it up once covers
every agent on the fleet.
"""

from __future__ import annotations

import json
import hashlib
import logging
import os
import ssl
import threading
import tempfile
import uuid
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Dict, List, Optional

from agent.memory_provider import MemoryProvider

logger = logging.getLogger(__name__)

_DEFAULT_GATEWAY_URL = ""
_CONFIG_FILENAME = "scos.json"

# Minimal fallback used only if the gateway can't be reached when building
# tool schemas, so the provider degrades to "recall/persist works, explicit
# tools don't" instead of registering nothing.
FALLBACK_TOOLS = [
    {
        "name": "scos_recall",
        "description": "Search SCOS Nexus memory for relevant context.",
        "inputSchema": {"type": "object", "properties": {"query": {"type": "string"}, "max_items": {"type": "integer"}}, "required": ["query"]},
    },
    {
        "name": "scos_status",
        "description": "Check SCOS Nexus memory provider status.",
        "inputSchema": {"type": "object", "properties": {}, "required": []},
    },
]


class ScosMemoryProvider(MemoryProvider):
    """SCOS Nexus Memory Fabric as a Hermes memory provider."""

    def __init__(self):
        self._env: Dict[str, str] = {}
        self._hermes_home: Optional[Path] = None
        self._session_id = ""
        self._sync_thread: Optional[threading.Thread] = None
        self._sync_lock = threading.Lock()
        self._persistence_warning_emitted = False

    @property
    def name(self) -> str:
        return "scos"

    # -- config ---------------------------------------------------------

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
                    values.update({str(k): str(v) for k, v in config.items() if v is not None})
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
            "SCOS_TENANT_ID and SCOS_PRINCIPAL_ID "
            f"in {self._env_path()} (see integrations/hermes/README.md)."
        )

    def get_config_schema(self) -> List[Dict[str, Any]]:
        return [
            {"key": "SCOS_GATEWAY_URL", "description": "SCOS gateway URL for recall and memory tools", "required": True},
            {"key": "SCOS_BEARER_TOKEN", "description": "Bearer token for mcp-gateway", "secret": True, "required": True, "env_var": "SCOS_BEARER_TOKEN"},
            {"key": "SCOS_TENANT_ID", "description": "SCOS tenant ID", "required": True},
            {"key": "SCOS_PRINCIPAL_ID", "description": "SCOS principal ID", "required": True},
            {"key": "SCOS_HERMES_PROVIDER_URL", "description": "SCOS memory persistence URL", "required": True},
            {"key": "SCOS_NAMESPACE_ID", "description": "Namespace for persisted turns", "required": True},
        ]

    def save_config(self, values: Dict[str, Any], hermes_home: str) -> None:
        """Persist non-secret setup values in Hermes' provider config."""
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
        unknown = sorted(set(values) - allowed - {"SCOS_BEARER_TOKEN", "SCOS_HERMES_BEARER_TOKEN"})
        if unknown:
            raise ValueError("unknown SCOS setup fields: " + ", ".join(unknown))

        target = Path(hermes_home) / _CONFIG_FILENAME
        target.parent.mkdir(parents=True, exist_ok=True)
        payload = {key: str(values[key]).strip() for key in allowed if values.get(key) is not None and str(values[key]).strip()}
        fd, temporary = tempfile.mkstemp(prefix=f".{_CONFIG_FILENAME}.", dir=str(target.parent))
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
        """Verify both memory surfaces before setup reports success."""
        self._hermes_home = Path(hermes_home)
        self._env = self._load_env()
        gateway = self._gateway_rpc("tools/list", {})
        if "error" in gateway:
            raise RuntimeError(f"SCOS recall check failed: {gateway['error'].get('message', gateway['error'])}")
        tools = (gateway.get("result") or {}).get("tools")
        if not isinstance(tools, list) or not any(item.get("name") == "scos_recall" for item in tools if isinstance(item, dict)):
            raise RuntimeError("SCOS recall check failed: gateway did not advertise scos_recall")

        provider_url = self._env.get("SCOS_HERMES_PROVIDER_URL", "").rstrip("/") + "/v1/provider/status"
        request = urllib.request.Request(provider_url, headers=self._provider_headers(), method="GET")
        try:
            with urllib.request.urlopen(request, timeout=8, context=self._ssl_context()) as response:
                status = json.loads(response.read().decode() or "{}")
        except Exception as error:
            raise RuntimeError(f"SCOS persistence check failed: {error}") from error
        if not status.get("healthy", status.get("is_available", False)):
            raise RuntimeError("SCOS persistence check failed: hermes-provider is not healthy")

    # -- lifecycle --------------------------------------------------------

    def initialize(self, session_id: str, **kwargs) -> None:
        hermes_home = kwargs.get("hermes_home")
        self._hermes_home = Path(hermes_home) if hermes_home else None
        self._session_id = session_id
        self._env = self._load_env()

    def system_prompt_block(self) -> str:
        if not self.is_available():
            return ""
        return (
            "# SCOS Nexus Memory\n"
            "Active. You have persistent cross-session memory via the SCOS Memory Fabric. "
            "Relevant memories are recalled automatically before each turn when available; "
            "you do not need to call a tool to search unless you need something the "
            "automatic recall missed — use scos_recall for that.\n"
        )

    def shutdown(self) -> None:
        if self._sync_thread and self._sync_thread.is_alive():
            self._sync_thread.join(timeout=5.0)

    # -- recall (prefetch) --------------------------------------------------

    def _ssl_context(self) -> ssl.SSLContext:
        ca_path = self._env.get("SCOS_TLS_CA_PATH", "")
        if ca_path and not Path(ca_path).exists():
            raise RuntimeError(f"SCOS_TLS_CA_PATH does not exist: {ca_path}")
        context = ssl.create_default_context(cafile=ca_path or None)
        if self._env.get("SCOS_TLS_INSECURE_SKIP_VERIFY") == "1":
            context.check_hostname = False
            context.verify_mode = ssl.CERT_NONE
        return context

    def _gateway_headers(self) -> Dict[str, str]:
        token = self._env.get("SCOS_BEARER_TOKEN") or self._env.get("SCOS_HERMES_BEARER_TOKEN", "")
        if not token or not self._env.get("SCOS_TENANT_ID") or not self._env.get("SCOS_PRINCIPAL_ID"):
            raise RuntimeError("SCOS_BEARER_TOKEN, SCOS_TENANT_ID and SCOS_PRINCIPAL_ID are required")
        return {
            "Content-Type": "application/json",
            "X-SCOS-Tenant-ID": self._env.get("SCOS_TENANT_ID", ""),
            "X-SCOS-Principal-ID": self._env.get("SCOS_PRINCIPAL_ID", ""),
            "Authorization": "Bearer " + token,
        }

    def _provider_headers(self) -> Dict[str, str]:
        """Headers for the hermes-provider (persistence) surface.

        The provider authenticates with the claims JWT — it rejects the
        mcp-gateway account key with 401 "authentication required", so the
        gateway token must never be sent here.
        """
        token = self._env.get("SCOS_HERMES_BEARER_TOKEN") or self._env.get("SCOS_BEARER_TOKEN", "")
        if not token or not self._env.get("SCOS_TENANT_ID") or not self._env.get("SCOS_PRINCIPAL_ID"):
            raise RuntimeError("a bearer token, SCOS_TENANT_ID and SCOS_PRINCIPAL_ID are required")
        return {
            "Content-Type": "application/json",
            "X-SCOS-Tenant-ID": self._env.get("SCOS_TENANT_ID", ""),
            "X-SCOS-Principal-ID": self._env.get("SCOS_PRINCIPAL_ID", ""),
            "Authorization": "Bearer " + token,
        }

    def _gateway_rpc(self, method: str, params: dict, timeout: float = 8) -> dict:
        url = self._env.get("SCOS_GATEWAY_URL")
        if not url:
            return {"error": {"code": -1, "message": "SCOS_GATEWAY_URL not configured"}}
        payload = {"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
        request = urllib.request.Request(
            url, data=json.dumps(payload).encode(), headers=self._gateway_headers(), method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=timeout, context=self._ssl_context()) as response:
                body = response.read().decode()
                return json.loads(body) if body else {}
        except urllib.error.HTTPError as error:
            return {"error": {"code": error.code, "message": error.read().decode(errors="replace")[:500]}}
        except Exception as error:
            return {"error": {"code": -1, "message": str(error)}}

    def prefetch(self, query: str, *, session_id: str = "") -> str:
        if not query or not self.is_available():
            return ""
        response = self._gateway_rpc("tools/call", {"name": "scos_recall", "arguments": {"query": query[:500]}})
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
        return f"## SCOS Nexus Memory\n{text[:4000]}"

    # -- persistence (sync_turn) --------------------------------------------

    def sync_turn(
        self,
        user_content: str,
        assistant_content: str,
        *,
        session_id: str = "",
        messages: Optional[List[Dict[str, Any]]] = None,
    ) -> None:
        base_url = self._env.get("SCOS_HERMES_PROVIDER_URL")
        token = self._env.get("SCOS_HERMES_BEARER_TOKEN") or self._env.get("SCOS_BEARER_TOKEN")
        namespace_id = self._env.get("SCOS_NAMESPACE_ID")
        if not (base_url and token and namespace_id and user_content and assistant_content):
            if not self._persistence_warning_emitted:
                logger.warning(
                    "SCOS turn persistence is not configured; set "
                    "SCOS_HERMES_PROVIDER_URL, SCOS_NAMESPACE_ID and a bearer token"
                )
                self._persistence_warning_emitted = True
            return

        sid = session_id or self._session_id or "unknown"

        def _sync():
            session_uuid = _stable_session_uuid(
                sid,
                self._env.get("SCOS_TENANT_ID", ""),
                self._env.get("SCOS_PRINCIPAL_ID", ""),
            )
            turn_id = hashlib.sha256(
                f"{sid}\0{user_content}\0{assistant_content}".encode("utf-8")
            ).hexdigest()
            turn_messages = messages or [
                {"role": "user", "content": user_content},
                {"role": "assistant", "content": assistant_content},
            ]
            body = json.dumps({
                "namespace_id": namespace_id,
                "session_id": session_uuid,
                "turn_id": turn_id,
                "messages": turn_messages,
                "tool_calls": [],
            }).encode()
            try:
                request = urllib.request.Request(
                    base_url.rstrip("/") + "/v1/provider/sync_turn",
                    data=body, method="POST",
                    headers=self._provider_headers(),
                )
                with urllib.request.urlopen(request, timeout=8, context=self._ssl_context()) as response:
                    if response.status // 100 != 2:
                        raise RuntimeError(f"unexpected sync status {response.status}")
            except Exception as error:
                logger.warning("SCOS sync_turn failed; turn not persisted: %s", error)

        with self._sync_lock:
            if self._sync_thread and self._sync_thread.is_alive():
                self._sync_thread.join(timeout=5.0)
            self._sync_thread = threading.Thread(target=_sync, daemon=True, name="scos-sync")
            self._sync_thread.start()

    # -- explicit tools (scos_recall, scos_status, ...) ---------------------

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
            schemas.append({
                "name": name,
                "description": tool.get("description", ""),
                "parameters": tool.get("inputSchema") or {"type": "object", "properties": {}, "required": []},
            })
        return schemas

    def handle_tool_call(self, tool_name: str, args: Dict[str, Any], **kwargs) -> str:
        del kwargs
        response = self._gateway_rpc("tools/call", {"name": tool_name, "arguments": args or {}})
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
    """Map Hermes' arbitrary session identifier to the provider UUID contract."""
    try:
        return str(uuid.UUID(session_id))
    except (ValueError, AttributeError, TypeError):
        identity = f"{tenant_id}\0{principal_id}\0{session_id}"
        return str(uuid.uuid5(uuid.NAMESPACE_URL, "scos-hermes-session:" + identity))


def register(ctx) -> None:
    """Register SCOS Nexus as a memory provider plugin."""
    ctx.register_memory_provider(ScosMemoryProvider())
