#!/usr/bin/env python3
"""
SCOS Nexus Memory Fabric — stdio-to-HTTP MCP relay for external agents
(cline, cursor, windsurf, antigravity, qwen, openclaw, continue, gemini, kilo,
codex, opencode, Claude Code, OpenAI tunnel).

This is a pure relay to mcp-gateway (services/mcp-gateway), which is the
single source of truth for the governed tool catalogue and owns all
per-tool backend routing (memory, code intelligence, agent-registry,
autonomous-control-plane, and every other governed service). The bridge
does not hardcode a tool list or talk to any backend service directly —
whatever mcp-gateway's tools/list returns is what gets served here, so the
tool surface never drifts out of date as the fabric grows. This mirrors
the stdio<->HTTP relay used by the Claude Desktop extension
(integrations/claude/desktop-extension/server/index.js). Installed to
~/.hermes/scripts/scos-mcp-bridge.py by scripts/install_agent_plugins.py; most
agent configs launch it via the thin per-client launchers alongside this file
(e.g. integrations/agent-plugins/gemini/scos-mcp-bridge.py).

Config comes from ~/.hermes/scos.env:
  SCOS_GATEWAY_URL        mcp-gateway MCP endpoint (default
                           https://scos.172.236.155.15.nip.io/mcp)
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
    "SCOS_GATEWAY_URL": "https://scos.172.236.155.15.nip.io/mcp",
    "SCOS_TENANT_ID": "11111111-2222-3333-4444-555555555555",
    "SCOS_PRINCIPAL_ID": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
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
GATEWAY_URL = ENV["SCOS_GATEWAY_URL"]
# Prefer SCOS_BEARER_TOKEN; fall back to the legacy per-service token name so
# existing ~/.hermes/scos.env files keep working without edits.
TOKEN = ENV.get("SCOS_BEARER_TOKEN") or ENV.get("SCOS_HERMES_BEARER_TOKEN", "")
TENANT_ID = ENV["SCOS_TENANT_ID"]
PRINCIPAL_ID = ENV["SCOS_PRINCIPAL_ID"]
CA_PATH = ENV.get("SCOS_TLS_CA_PATH", "")

AGENT_NAME = os.getenv("SCOS_AGENT_NAME", "external-agent")


def log(msg):
    sys.stderr.write(f"[scos-mcp-bridge:{AGENT_NAME}] {msg}\n")
    sys.stderr.flush()


def ssl_ctx():
    ctx = ssl.create_default_context(cafile=CA_PATH) if CA_PATH and os.path.exists(CA_PATH) else ssl.create_default_context()
    ctx.check_hostname = False
    # The internal cert is a self-signed leaf (O=scos-internal-dev); the
    # "CA" file at ~/.hermes/scos-ca.crt is that same leaf, so it can never
    # verify as a trust anchor. This is an internal tailnet service — skip
    # peer verification entirely.
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


def gateway_headers():
    h = {
        "Content-Type": "application/json",
        "X-SCOS-Tenant-ID": TENANT_ID,
        "X-SCOS-Principal-ID": PRINCIPAL_ID,
        "X-SCOS-Agent-Name": AGENT_NAME,
    }
    if TOKEN:
        h["Authorization"] = "Bearer " + TOKEN
    return h


def relay(rpc_request, timeout=30.0):
    """POST one JSON-RPC message to mcp-gateway and return the decoded
    response, or a JSON-RPC error object on transport failure."""
    req = urllib.request.Request(
        GATEWAY_URL,
        data=json.dumps(rpc_request).encode(),
        headers=gateway_headers(),
        method="POST",
    )
    try:
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


log(f"relaying to {GATEWAY_URL}")

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

    is_notification = "id" not in msg
    response = relay(msg)

    if response is None or is_notification:
        continue

    if os.environ.get("SCOS_MCP_DEBUG"):
        sys.stderr.write(f"[scos-mcp-bridge:debug] SENT: {json.dumps(response)[:200]}\n")
        sys.stderr.flush()
    sys.stdout.write(json.dumps(response) + "\n")
    sys.stdout.flush()
