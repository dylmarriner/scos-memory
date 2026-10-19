#!/usr/bin/env python3
"""
SCOS Nexus Memory Fabric — stdio-to-HTTP MCP relay for external agents
(kilo, codex, opencode, Claude Code, OpenAI tunnel).

This is a pure relay to mcp-gateway (services/mcp-gateway), which is the
single source of truth for the governed tool catalogue and owns all
per-tool backend routing (memory, code intelligence, agent-registry,
autonomous-control-plane, and every other governed service). The bridge
does not hardcode a tool list or talk to any backend service directly —
whatever mcp-gateway's tools/list returns is what gets served here, so the
tool surface never drifts out of date as the fabric grows. This mirrors
the stdio<->HTTP relay used by the Claude Desktop extension
(integrations/claude/desktop-extension/server/index.js).

Config comes from ~/.hermes/scos.env:
  SCOS_GATEWAY_URL        mcp-gateway MCP endpoint (default
                           localhost development endpoint; set this for production)
  SCOS_BEARER_TOKEN        internal service token / claims JWT (falls back
                           to the legacy SCOS_HERMES_BEARER_TOKEN name)
  SCOS_TENANT_ID / SCOS_PRINCIPAL_ID  identity
  SCOS_TLS_CA_PATH         CA for TLS verification (optional)

Route logs to stderr so stdout stays protocol-clean.
"""
import json
import os
import ssl
import sys
import urllib.error
import urllib.request

ENV_PATH = os.path.expanduser("~/.hermes/scos.env")

DEFAULTS = {
    "SCOS_GATEWAY_URL": "https://127.0.0.1:8098/mcp",
    "SCOS_TLS_CA_PATH": "",
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
GATEWAY_URL = ENV["SCOS_GATEWAY_URL"]
# Prefer SCOS_BEARER_TOKEN; fall back to the legacy per-service token name so
# existing ~/.hermes/scos.env files keep working without edits.
TOKEN = ENV.get("SCOS_BEARER_TOKEN") or ENV.get("SCOS_HERMES_BEARER_TOKEN", "")
TENANT_ID = ENV.get("SCOS_TENANT_ID", "")
PRINCIPAL_ID = ENV.get("SCOS_PRINCIPAL_ID", "")
CA_PATH = ENV.get("SCOS_TLS_CA_PATH", "")

AGENT_NAME = os.getenv("SCOS_AGENT_NAME", "external-agent")


def log(msg):
    sys.stderr.write(f"[scos-mcp-bridge:{AGENT_NAME}] {msg}\n")
    sys.stderr.flush()


def ssl_ctx():
    if CA_PATH and not os.path.exists(CA_PATH):
        raise RuntimeError(f"SCOS_TLS_CA_PATH does not exist: {CA_PATH}")
    ctx = ssl.create_default_context(cafile=CA_PATH or None)
    if os.environ.get("SCOS_TLS_INSECURE_SKIP_VERIFY") == "1":
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    return ctx


def gateway_headers():
    if not TOKEN or not TENANT_ID or not PRINCIPAL_ID:
        raise RuntimeError("SCOS_BEARER_TOKEN, SCOS_TENANT_ID and SCOS_PRINCIPAL_ID are required")
    h = {
        "Content-Type": "application/json",
        "X-SCOS-Tenant-ID": TENANT_ID,
        "X-SCOS-Principal-ID": PRINCIPAL_ID,
        "X-SCOS-Agent-Name": AGENT_NAME,
    }
    if TOKEN:
        h["Authorization"] = "Bearer " + TOKEN
    return h


# Tool calls that do bounded but long work upstream (taking a backup,
# running a restore drill); mcp-gateway gives them 30 minutes, so the bridge
# must not give up first.
LONG_TOOLS = {"scos_backup_start", "scos_restore_test_start", "scos_okf_mirror_push", "scos_backup_plan", "scos_memory_import"}


def call_timeout(msg):
    # params may legally be an array; only a tools/call object names a tool.
    params = msg.get("params")
    if msg.get("method") == "tools/call" and isinstance(params, dict) and params.get("name") in LONG_TOOLS:
        return 1800.0
    return 30.0


def relay(rpc_request, timeout=30.0):
    """POST one JSON-RPC message to mcp-gateway and return the decoded
    response, or a JSON-RPC error object on transport failure."""
    try:
        req = urllib.request.Request(
            GATEWAY_URL,
            data=json.dumps(rpc_request).encode(),
            headers=gateway_headers(),
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=timeout, context=ssl_ctx()) as resp:
            body = resp.read().decode()
            if not body:
                return None
            return json.loads(body)
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")
        log(f"mcp-gateway HTTP {e.code}: {body[:300]}")
        return {
            "jsonrpc": "2.0",
            "id": rpc_request.get("id"),
            "error": {"code": -32000, "message": f"mcp-gateway HTTP {e.code}: {body[:500]}"},
        }
    except Exception as e:
        log(f"mcp-gateway transport error: {e}")
        return {
            "jsonrpc": "2.0",
            "id": rpc_request.get("id"),
            "error": {"code": -32001, "message": f"mcp-gateway unreachable: {e}"},
        }



def rpc_error(msg_id, code, message):
    return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": code, "message": message}}


def handle(msg):
    """Relay one JSON-RPC message; returns the response to write, or None
    for a notification (which gets no response)."""
    if not isinstance(msg, dict):
        return rpc_error(None, -32600, "invalid request: a JSON-RPC message must be an object")
    is_notification = "id" not in msg
    response = relay(msg, timeout=call_timeout(msg))
    if response is None or is_notification:
        return None
    return response


def handle_line(line):
    """The response to one line of input: an object for a single message,
    an array for a batch (one response per request, in order, notifications
    omitted), or None when nothing is owed."""
    try:
        msg = json.loads(line)
    except json.JSONDecodeError as e:
        return rpc_error(None, -32700, f"parse error: {e}")
    if isinstance(msg, list):
        if not msg:
            return rpc_error(None, -32600, "invalid request: empty batch")
        responses = [r for r in (handle(m) for m in msg) if r is not None]
        return responses or None
    return handle(msg)


if __name__ == "__main__":
    log(f"relaying to {GATEWAY_URL}")
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        if os.environ.get("SCOS_MCP_DEBUG"):
            sys.stderr.write(f"[scos-mcp-bridge:debug] GOT: {line}\n")
            sys.stderr.flush()
        response = handle_line(line)
        if response is None:
            continue
        if os.environ.get("SCOS_MCP_DEBUG"):
            sys.stderr.write(f"[scos-mcp-bridge:debug] SENT: {json.dumps(response)[:200]}\n")
            sys.stderr.flush()
        sys.stdout.write(json.dumps(response) + "\n")
        sys.stdout.flush()
