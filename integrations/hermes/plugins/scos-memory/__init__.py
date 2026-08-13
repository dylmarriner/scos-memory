"""SCOS Memory Fabric plugin for Hermes Agent.

The plugin calls hermes-provider directly and reads connection settings from
the shared ~/.hermes/scos.env file used by the other SCOS integrations.
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


def _load_env():
    values = {
        "SCOS_HERMES_PROVIDER_URL": "https://100.68.0.96:8088",
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


def _call(tool, arguments):
    env = _load_env()
    payload = {
        "session_id": SESSION_ID,
        "tenant_id": env.get("SCOS_TENANT_ID", ""),
        "principal_id": env.get("SCOS_PRINCIPAL_ID", ""),
        "tool": tool,
        "arguments": dict(arguments or {}),
    }
    if tool == "scos_recall":
        payload["arguments"].setdefault("session_id", SESSION_ID)
    request = urllib.request.Request(
        env["SCOS_HERMES_PROVIDER_URL"].rstrip("/") + "/v1/provider/tool_call",
        data=json.dumps(payload).encode(),
        headers={
            "Content-Type": "application/json",
            "X-SCOS-Tenant-ID": env.get("SCOS_TENANT_ID", ""),
            "X-SCOS-Principal-ID": env.get("SCOS_PRINCIPAL_ID", ""),
            **({"Authorization": "Bearer " + env["SCOS_HERMES_BEARER_TOKEN"]}
               if env.get("SCOS_HERMES_BEARER_TOKEN") else {}),
        },
        method="POST",
    )
    ca_path = env.get("SCOS_TLS_CA_PATH", "")
    context = ssl.create_default_context(cafile=ca_path) if ca_path and Path(ca_path).exists() else ssl.create_default_context()
    if not (ca_path and Path(ca_path).exists()):
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE
    try:
        with urllib.request.urlopen(request, timeout=30, context=context) as response:
            return json.loads(response.read().decode() or "{}")
    except urllib.error.HTTPError as error:
        return {"error": {"code": error.code, "message": error.read().decode(errors="replace")[:500]}}
    except Exception as error:
        return {"error": {"code": -1, "message": str(error)}}


def _handler(tool):
    def handle(arguments, **kwargs):
        del kwargs
        return json.dumps(_call(tool, arguments))
    return handle


TOOLS = [
    ("scos_recall", "Search SCOS Nexus memory for relevant context.", {"query": {"type": "string"}, "max_items": {"type": "integer"}}, ["query"]),
    ("scos_status", "Check SCOS Nexus memory provider status.", {}, []),
    ("scos_explain", "Explain why a memory is held and what evidence supports it.", {"memory_id": {"type": "string"}}, ["memory_id"]),
    ("scos_user_model", "Inspect the stored SCOS user representation.", {}, []),
    ("scos_memory_graph", "Inspect SCOS memory entities and relationships.", {}, []),
    ("scos_correct", "Correct a specific stored memory after confirming its exact memory_id.", {"memory_id": {"type": "string"}, "value": {"type": "string"}}, ["memory_id", "value"]),
    ("scos_forget", "Remove a specific stored memory after confirming its exact memory_id.", {"memory_id": {"type": "string"}}, ["memory_id"]),
]


def register(ctx):
    for name, description, properties, required in TOOLS:
        ctx.register_tool(
            name=name,
            toolset="scos_memory",
            schema={
                "name": name,
                "description": description,
                "parameters": {"type": "object", "properties": properties, "required": required},
            },
            handler=_handler(name),
            description=description,
        )
