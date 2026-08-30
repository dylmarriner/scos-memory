"""SCOS Memory Fabric plugin for Hermes Agent.

The plugin calls mcp-gateway (services/mcp-gateway) — the single source of
truth for the governed tool catalogue — rather than talking to any one
backend service directly. At registration time it fetches the live tool
list from mcp-gateway's tools/list and registers each one generically, so
the tool surface here never drifts behind the fabric as it grows (it used
to hardcode a 7-tool memory-only list pointed at hermes-provider). If the
gateway is unreachable at startup, a small static fallback list keeps the
plugin usable in a degraded state.

Reads connection settings from the shared ~/.hermes/scos.env file used by
the other SCOS integrations.
"""

import json
import os
import ssl
import urllib.error
import urllib.request
import uuid
from pathlib import Path


ENV_PATH = Path.home() / ".hermes" / "scos.env"
SESSION_ID = str(uuid.uuid5(uuid.NAMESPACE_DNS, "scos-hermes-agent"))

# Minimal fallback used only if the gateway can't be reached at registration
# time, so the plugin degrades gracefully instead of registering nothing.
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


def _load_env():
    values = {
        "SCOS_GATEWAY_URL": "https://scos.172.236.155.15.nip.io/mcp",
        "SCOS_TLS_CA_PATH": str(Path.home() / ".hermes" / "scos-ca.crt"),
    }
    if ENV_PATH.exists():
        for line in ENV_PATH.read_text().splitlines():
            line = line.strip()
            if line and "=" in line and not line.startswith("#"):
                key, value = line.split("=", 1)
                values[key] = value
    values.update({key: value for key, value in os.environ.items() if key.startswith("SCOS_")})
    return values


def _ssl_context(env):
    ca_path = env.get("SCOS_TLS_CA_PATH", "")
    context = ssl.create_default_context(cafile=ca_path) if ca_path and Path(ca_path).exists() else ssl.create_default_context()
    if not (ca_path and Path(ca_path).exists()):
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE
    return context


def _gateway_headers(env):
    headers = {
        "Content-Type": "application/json",
        "X-SCOS-Tenant-ID": env.get("SCOS_TENANT_ID", ""),
        "X-SCOS-Principal-ID": env.get("SCOS_PRINCIPAL_ID", ""),
    }
    token = env.get("SCOS_BEARER_TOKEN") or env.get("SCOS_HERMES_BEARER_TOKEN", "")
    if token:
        headers["Authorization"] = "Bearer " + token
    return headers


def _gateway_rpc(method, params, timeout=30):
    env = _load_env()
    payload = {"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
    request = urllib.request.Request(
        env["SCOS_GATEWAY_URL"],
        data=json.dumps(payload).encode(),
        headers=_gateway_headers(env),
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout, context=_ssl_context(env)) as response:
            body = response.read().decode()
            return json.loads(body) if body else {}
    except urllib.error.HTTPError as error:
        return {"error": {"code": error.code, "message": error.read().decode(errors="replace")[:500]}}
    except Exception as error:
        return {"error": {"code": -1, "message": str(error)}}


def _call(tool, arguments):
    args = dict(arguments or {})
    if tool == "scos_recall":
        args.setdefault("session_id", SESSION_ID)
    response = _gateway_rpc("tools/call", {"name": tool, "arguments": args})
    if "error" in response:
        return {"error": response["error"]}
    result = response.get("result", {})
    content = result.get("content") or []
    for block in content:
        if block.get("type") == "text":
            try:
                return json.loads(block["text"])
            except (KeyError, json.JSONDecodeError):
                return {"text": block.get("text")}
    return result


def _handler(tool):
    def handle(arguments, **kwargs):
        del kwargs
        return json.dumps(_call(tool, arguments))
    return handle


def _discover_tools():
    response = _gateway_rpc("tools/list", {}, timeout=5)
    tools = (response.get("result") or {}).get("tools")
    if isinstance(tools, list) and tools:
        return tools
    return FALLBACK_TOOLS


def register(ctx):
    for tool in _discover_tools():
        name = tool.get("name")
        if not name:
            continue
        description = tool.get("description", "")
        schema = tool.get("inputSchema") or {"type": "object", "properties": {}, "required": []}
        ctx.register_tool(
            name=name,
            toolset="scos_memory",
            schema={"name": name, "description": description, "parameters": schema},
            handler=_handler(name),
            description=description,
        )
