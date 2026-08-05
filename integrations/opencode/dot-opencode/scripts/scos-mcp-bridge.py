#!/usr/bin/env python3
"""
SCOS Nexus Memory Fabric — stdio MCP bridge for external agents (kilo, codex,
Claude Code, OpenAI tunnel). Talks to the SCOS hermes-provider on rabuntu and
exposes the 7 governed tools (scos_recall, scos_status, scos_explain,
scos_user_model, scos_memory_graph, scos_correct, scos_forget) over stdio MCP.

Config comes from ~/.hermes/scos.env:
  SCOS_HERMES_PROVIDER_URL  base URL (default https://100.68.0.96:8088)
  SCOS_HERMES_BEARER_TOKEN  internal service token
  SCOS_TENANT_ID / SCOS_PRINCIPAL_ID / SCOS_NAMESPACE_ID  identity
  SCOS_TLS_CA_PATH          CA for TLS verification (optional)

For scos_recall the provider requires a UUID session_id; external agents don't
send one, so this bridge maintains a stable UUID per agent (SCOS_AGENT_NAME or
argv) so repeated recalls within a session hit the same session bucket.

Route logs to stderr so stdout stays protocol-clean.
"""
import json
import os
import ssl
import sys
import uuid
import urllib.error
import urllib.request

ENV_PATH = os.path.expanduser("~/.hermes/scos.env")

DEFAULTS = {
    "SCOS_HERMES_PROVIDER_URL": "https://100.68.0.96:8088",
    "SCOS_TENANT_ID": "11111111-2222-3333-4444-555555555555",
    "SCOS_PRINCIPAL_ID": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
    "SCOS_NAMESPACE_ID": "bbbbbbbb-1111-2222-3333-444444444444",
    "SCOS_TLS_CA_PATH": os.path.expanduser("~/.hermes/scos-ca.crt"),
}


def load_env():
    env = dict(DEFAULTS)
    if os.path.exists(ENV_PATH):
        with open(ENV_PATH) as f:
            for line in f:
                line = line.strip()
                if line and "=" in line and not line.startswith("#"):
                    k, v = line.split("=", 1)
                    env[k] = v
    # Process environment wins
    for k in list(env.keys()):
        if os.getenv(k):
            env[k] = os.getenv(k)
    return env


ENV = load_env()
BASE_URL = ENV["SCOS_HERMES_PROVIDER_URL"].rstrip("/")
TOKEN = ENV.get("SCOS_HERMES_BEARER_TOKEN", "")
TENANT_ID = ENV["SCOS_TENANT_ID"]
PRINCIPAL_ID = ENV["SCOS_PRINCIPAL_ID"]
CA_PATH = ENV.get("SCOS_TLS_CA_PATH", "")

# Stable per-agent session UUID: deterministic from agent name so external
# clients get a consistent session bucket across turns.
AGENT_NAME = os.getenv("SCOS_AGENT_NAME", "external-agent")
SESSION_UUID = str(uuid.uuid5(uuid.NAMESPACE_DNS, f"scos-mcp-{AGENT_NAME}"))


def log(msg):
    sys.stderr.write(f"[scos-mcp-bridge:{AGENT_NAME}] {msg}\n")
    sys.stderr.flush()


def ssl_ctx():
    ctx = ssl.create_default_context(cafile=CA_PATH) if CA_PATH and os.path.exists(CA_PATH) else ssl.create_default_context()
    ctx.check_hostname = False
    if not (CA_PATH and os.path.exists(CA_PATH)):
        ctx.verify_mode = ssl.CERT_NONE
    return ctx


def headers():
    h = {
        "Content-Type": "application/json",
        "X-SCOS-Tenant-ID": TENANT_ID,
        "X-SCOS-Principal-ID": PRINCIPAL_ID,
    }
    if TOKEN:
        h["Authorization"] = "Bearer " + TOKEN
    return h


def provider_post(path, payload, timeout=30.0):
    req = urllib.request.Request(
        BASE_URL + path,
        data=json.dumps(payload or {}).encode(),
        headers=headers(),
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=ssl_ctx()) as resp:
            body = resp.read().decode()
            return json.loads(body) if body else {}
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")
        log(f"{path} HTTP {e.code}: {body[:300]}")
        return {"error": {"code": e.code, "message": body[:500]}}
    except Exception as e:
        log(f"{path} transport error: {e}")
        return {"error": {"code": -1, "message": str(e)}}


# ── Tool discovery ──────────────────────────────────────────────────
# Static governed tool list (server-defined in governedToolPaths) so the
# bridge answers tools/list instantly — external clients (kilo, codex,
# Claude Code) have short MCP startup timeouts. A live refresh from the
# provider runs once at startup; if it fails we keep the static list.
STATIC_TOOLS = [
    {
        "description": "Search SCOS Nexus memory for relevant context",
        "inputSchema": {
            "properties": {
                "max_items": {"description": "Max results", "type": "integer"},
                "query": {"description": "Search query", "type": "string"},
            },
            "required": ["query"],
            "type": "object",
        },
        "name": "scos_recall",
    },
    {
        "description": "Check SCOS Nexus memory provider status",
        "inputSchema": {"properties": {}, "type": "object"},
        "name": "scos_status",
    },
    {
        "description": "Request removal of a specific memory",
        "inputSchema": {
            "properties": {"memory_id": {"description": "Memory ID to forget", "type": "string"}},
            "required": ["memory_id"],
            "type": "object",
        },
        "name": "scos_forget",
    },
    {
        "description": "Explain why a memory is held and what evidence supports it",
        "inputSchema": {
            "properties": {"memory_id": {"description": "Memory ID to explain", "type": "string"}},
            "required": ["memory_id"],
            "type": "object",
        },
        "name": "scos_explain",
    },
    {
        "description": "Inspect the stored user representation",
        "inputSchema": {"properties": {}, "type": "object"},
        "name": "scos_user_model",
    },
    {
        "description": "Inspect memory entities and their relationships",
        "inputSchema": {"properties": {}, "type": "object"},
        "name": "scos_memory_graph",
    },
    {
        "description": "Correct a stored memory, preserving the superseded version",
        "inputSchema": {
            "properties": {
                "memory_id": {"description": "Memory ID to correct", "type": "string"},
                "value": {"description": "Corrected value", "type": "string"},
            },
            "required": ["memory_id", "value"],
            "type": "object",
        },
        "name": "scos_correct",
    },
]


TOOLS = STATIC_TOOLS
log(f"serving {len(TOOLS)} governed tools (static; lazy refresh on tools/list)")


def _refresh_once():
    """Background refresh of tool schemas from the provider. Never blocks the
    handshake — the static list is served immediately; live schemas land when
    the provider responds. Normalizes snake_case input_schema to the
    spec-compliant camelCase inputSchema."""
    try:
        resp = provider_post("/v1/provider/tool_schemas", {}, timeout=5.0)
        tools = []
        if isinstance(resp, list) and resp:
            tools = resp
        elif isinstance(resp, dict) and resp.get("tools"):
            tools = resp["tools"]
        normalized = []
        for t in tools:
            if isinstance(t, dict) and "input_schema" in t and "inputSchema" not in t:
                t = dict(t)
                t["inputSchema"] = t["input_schema"]
                t.pop("input_schema", None)
            normalized.append(t)
        return normalized or STATIC_TOOLS
    except Exception as e:
        log(f"live tool refresh failed, using static list: {e}")
    return STATIC_TOOLS


_refreshed = False


def current_tools():
    """Static list is the default. A live refresh is attempted at most once,
    lazily, but the client always gets an instant answer first."""
    global _refreshed, TOOLS
    if not _refreshed:
        _refreshed = True
        try:
            import threading
            threading.Thread(target=lambda: setattr(sys.modules[__name__], "TOOLS", _refresh_once()), daemon=True).start()
        except Exception:
            pass
    return TOOLS


def handle_tool_call(name, arguments):
    args = dict(arguments or {})
    if name == "scos_recall":
        # Provider requires a UUID session_id; inject stable per-agent UUID.
        args.setdefault("session_id", SESSION_UUID)
    resp = provider_post("/v1/provider/tool_call", {
        "session_id": args.pop("session_id", SESSION_UUID),
        "tenant_id": TENANT_ID,
        "principal_id": PRINCIPAL_ID,
        "tool": name,
        "arguments": args,
    })
    if "error" in resp:
        return None, resp["error"]
    return resp, None


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    if os.environ.get("SCOS_MCP_DEBUG"):
        sys.stderr.write(f"[scos-mcp-bridge:debug] GOT: {line}\n")
        sys.stderr.flush()
    try:
        msg = json.loads(line)
    except json.JSONDecodeError:
        continue

    method = msg.get("method", "")
    msg_id = msg.get("id")
    params = msg.get("params", {})
    response = None

    if method == "initialize":
        # Echo back the client's requested protocol version — newer clients
        # (kilo/opencode 7.x) send 2025-11-25 and hard-fail on version mismatch
        # if we reply with an older one.
        client_version = (params or {}).get("protocolVersion", "2024-11-05")
        response = {
            "jsonrpc": "2.0",
            "id": msg_id,
            "result": {
                "protocolVersion": client_version,
                "serverInfo": {"name": "scos-memory", "version": "0.1.0"},
                "capabilities": {"tools": {"listChanged": True}},
            },
        }
    elif method == "tools/list":
        response = {"jsonrpc": "2.0", "id": msg_id, "result": {"tools": current_tools()}}
    elif method == "tools/call":
        name = params.get("name", "")
        result, err = handle_tool_call(name, params.get("arguments", {}))
        if err is not None:
            response = {"jsonrpc": "2.0", "id": msg_id, "error": err}
        else:
            # Wrap content per MCP spec
            content = result if isinstance(result, dict) else {"result": result}
            response = {"jsonrpc": "2.0", "id": msg_id, "result": {"content": [{"type": "text", "text": json.dumps(content, default=str)}]}}
    elif method == "notifications/initialized":
        continue
    elif method == "ping":
        response = {"jsonrpc": "2.0", "id": msg_id, "result": {}}
    else:
        response = {"jsonrpc": "2.0", "id": msg_id, "result": {}}

    if response is not None:
        if os.environ.get("SCOS_MCP_DEBUG"):
            sys.stderr.write(f"[scos-mcp-bridge:debug] SENT: {json.dumps(response)[:200]}\n")
            sys.stderr.flush()
        sys.stdout.write(json.dumps(response) + "\n")
        sys.stdout.flush()
