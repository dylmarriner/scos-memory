#!/usr/bin/env python3
"""Report the real (runtime-verified where possible) integration status of
every agent listed in integrations/agent-plugins/registry.json.

GOVERNANCE.md 4.20/4.17 requires truthful, evidence-backed status - this
deliberately does not infer "working" from a file existing. It distinguishes
three states per agent:

  VERIFIED   - a live memory-blind round trip (separate process, a fact
               stated with no memory/SCOS wording, recalled in a second
               separate process) has been run for this agent (see
               docs/agent-connection-guide.md), AND on this machine the
               agent binary is installed, the SCOS integration is written
               to its configuration, and the SCOS MCP gateway that
               configuration points at answers an MCP tools/list request.
               Every one of those checks runs each time the doctor runs.
  PRESENT    - the machine shows real evidence (binary/config dir), but
               either no automatic-lifecycle test has been run (tier is
               unconfirmed, not assumed Tier A) or one of the VERIFIED
               checks failed here; failed checks are listed per agent.
  MISSING    - registry claims an integration but no local evidence found.

Extend the LIFECYCLE_TESTED dict below only after actually running the
memory-blind test for that agent id, and give it an integration check in
INTEGRATION_CHECKS - do not mark an agent verified because its hooks.json
merely exists.
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import ssl
import sys
import tomllib
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable, Optional

ROOT = Path(__file__).resolve().parents[1]
REGISTRY_PATH = ROOT / "integrations" / "agent-plugins" / "registry.json"

# agent_id -> (tier, evidence note) for agents whose memory-blind lifecycle
# test has actually been run. Being listed here is necessary but not
# sufficient for VERIFIED: the per-machine checks below must pass too.
LIFECYCLE_TESTED = {
    "claude-code": (
        "Tier B",
        "UserPromptSubmit auto-recall + Stop auto-persist verified via two "
        "separate `claude -p` sessions (session A stated a fact with no "
        "memory wording, session B recalled it with no explicit tool call).",
    ),
    "codex": (
        "Tier B",
        "Same test via two separate `codex exec` sessions; required a "
        "Codex-specific transcript parser (different rollout schema from "
        "Claude Code's JSONL).",
    ),
}

# Plugin key both native plugins are installed under
# (scripts/install_agent_plugins.py).
SCOS_PLUGIN_KEY = "scos-memory-fabric@scos-fabric-tools"

# The bridge's default gateway when ~/.hermes/scos.env does not set one
# (integrations/agent-plugins/scos-mcp-bridge.py).
DEFAULT_GATEWAY_URL = "https://127.0.0.1:8098/mcp"

GATEWAY_PROBE_TIMEOUT = 5.0

# Binary names / config dirs that indicate a real local install, used only
# for the PRESENT/MISSING distinction - never to imply a tier.
PRESENCE_CHECKS = {
    "claude-code": (["claude"], ["~/.claude"]),
    "codex": (["codex"], ["~/.codex"]),
    "hermes-agent": (["hermes"], ["~/.hermes"]),
    "kilo": (["kilo"], ["~/.config/kilo"]),
    "opencode": ([], ["~/.opencode"]),
    "openclaw": (["openclaw"], ["~/.openclaw"]),
    "qwen-code": (["qwen"], ["~/.qwen"]),
    "gemini-cli": (["gemini"], ["~/.gemini"]),
    "cline": (["cline"], ["~/.cline"]),
    "cursor": (["cursor"], ["~/.cursor"]),
    "windsurf-devin": (["windsurf"], ["~/.windsurf", "~/.devin"]),
    "antigravity": (["antigravity"], ["~/.antigravity"]),
    "continue": ([], ["~/.continue"]),
    "aider": ([], ["~/.aider", "~/.aider.conf.yml"]),
}


@dataclass
class GatewayTarget:
    """Where an installed integration sends MCP traffic, and with what headers."""

    url: str
    headers: dict
    ca_path: str = ""


@dataclass
class Check:
    name: str
    passed: bool
    detail: str

    def as_dict(self) -> dict:
        return {"name": self.name, "passed": self.passed, "detail": self.detail}


def _expand(path: str, home: Path) -> Path:
    return home / path[2:] if path.startswith("~/") else Path(path)


def _read_json(path: Path) -> Optional[dict]:
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError):
        return None
    return data if isinstance(data, dict) else None


def load_scos_env(home: Path) -> dict:
    """Read ~/.hermes/scos.env the way the stdio bridge does."""
    env = {}
    path = home / ".hermes" / "scos.env"
    try:
        lines = path.read_text().splitlines()
    except OSError:
        return env
    for line in lines:
        line = line.strip()
        if line and "=" in line and not line.startswith("#"):
            key, value = line.split("=", 1)
            env[key] = value
    return env


def bridge_target(home: Path) -> GatewayTarget:
    """The gateway and headers the stdio bridge would use on this machine.

    As in the bridge, a variable set in the process environment wins over
    ~/.hermes/scos.env.
    """
    env = load_scos_env(home)
    for key in ("SCOS_GATEWAY_URL", "SCOS_MCP_GATEWAY_URL", "SCOS_BEARER_TOKEN", "SCOS_HERMES_BEARER_TOKEN",
                "SCOS_TENANT_ID", "SCOS_PRINCIPAL_ID", "SCOS_TLS_CA_PATH"):
        if os.environ.get(key):
            env[key] = os.environ[key]
    url = env.get("SCOS_GATEWAY_URL") or env.get("SCOS_MCP_GATEWAY_URL") or DEFAULT_GATEWAY_URL
    token = env.get("SCOS_BEARER_TOKEN") or env.get("SCOS_HERMES_BEARER_TOKEN", "")
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    if env.get("SCOS_TENANT_ID"):
        headers["X-SCOS-Tenant-ID"] = env["SCOS_TENANT_ID"]
    if env.get("SCOS_PRINCIPAL_ID"):
        headers["X-SCOS-Principal-ID"] = env["SCOS_PRINCIPAL_ID"]
    return GatewayTarget(url=url, headers=headers, ca_path=env.get("SCOS_TLS_CA_PATH", ""))


def _plugin_installed_claude(home: Path) -> bool:
    settings = _read_json(home / ".claude" / "settings.json") or {}
    enabled = settings.get("enabledPlugins")
    if isinstance(enabled, dict) and enabled.get(SCOS_PLUGIN_KEY) is True:
        return True
    installed = _read_json(home / ".claude" / "plugins" / "installed_plugins.json") or {}
    plugins = installed.get("plugins")
    return isinstance(plugins, dict) and bool(plugins.get(SCOS_PLUGIN_KEY))


def claude_code_integration(home: Path) -> tuple[Optional[GatewayTarget], str]:
    """Find the SCOS MCP server Claude Code is configured with.

    Either the native plugin is installed and enabled (its .mcp.json takes
    the gateway from the plugin's userConfig), or a user-scope `scos` MCP
    server is written to ~/.claude.json.
    """
    if _plugin_installed_claude(home):
        settings = _read_json(home / ".claude" / "settings.json") or {}
        options = (
            (settings.get("pluginConfigs") or {}).get(SCOS_PLUGIN_KEY, {}).get("options", {})
        )
        url = options.get("gateway_url") if isinstance(options, dict) else None
        if not url:
            return None, f"{SCOS_PLUGIN_KEY} is installed but has no gateway_url configured"
        headers = {"Content-Type": "application/json"}
        if options.get("bearer_token"):
            headers["Authorization"] = "Bearer " + options["bearer_token"]
        if options.get("tenant_id"):
            headers["X-SCOS-Tenant-ID"] = options["tenant_id"]
        if options.get("principal_id"):
            headers["X-SCOS-Principal-ID"] = options["principal_id"]
        ca_path = load_scos_env(home).get("SCOS_TLS_CA_PATH", "")
        return GatewayTarget(url=url, headers=headers, ca_path=ca_path), f"plugin {SCOS_PLUGIN_KEY} enabled"

    servers = (_read_json(home / ".claude.json") or {}).get("mcpServers") or {}
    scos = servers.get("scos") if isinstance(servers, dict) else None
    if isinstance(scos, dict):
        if scos.get("url"):
            headers = {"Content-Type": "application/json", **(scos.get("headers") or {})}
            return GatewayTarget(url=scos["url"], headers=headers), "mcpServers.scos in ~/.claude.json"
        if scos.get("command"):
            return bridge_target(home), "mcpServers.scos (stdio bridge) in ~/.claude.json"
    return None, f"neither plugin {SCOS_PLUGIN_KEY} nor an `scos` MCP server is configured"


def codex_integration(home: Path) -> tuple[Optional[GatewayTarget], str]:
    """Find the SCOS MCP server Codex is configured with in ~/.codex/config.toml.

    Both the packaged plugin and a manual [mcp_servers.scos] entry launch the
    stdio bridge, which reads its gateway from ~/.hermes/scos.env.
    """
    path = home / ".codex" / "config.toml"
    try:
        config = tomllib.loads(path.read_text())
    except OSError:
        return None, f"{path} not found"
    except tomllib.TOMLDecodeError as exc:
        return None, f"{path} is not valid TOML: {exc}"
    plugin = (config.get("plugins") or {}).get(SCOS_PLUGIN_KEY)
    if isinstance(plugin, dict) and plugin.get("enabled", True) is not False:
        return bridge_target(home), f"plugin {SCOS_PLUGIN_KEY} enabled"
    server = (config.get("mcp_servers") or {}).get("scos")
    if isinstance(server, dict) and server.get("enabled", True) is not False:
        if server.get("url"):
            headers = {"Content-Type": "application/json", **(server.get("http_headers") or {})}
            return GatewayTarget(url=server["url"], headers=headers), "[mcp_servers.scos] in config.toml"
        return bridge_target(home), "[mcp_servers.scos] (stdio bridge) in config.toml"
    return None, f"neither plugin {SCOS_PLUGIN_KEY} nor [mcp_servers.scos] is enabled in {path}"


# agent_id -> function returning the configured gateway (or None and why).
INTEGRATION_CHECKS: dict[str, Callable[[Path], tuple[Optional[GatewayTarget], str]]] = {
    "claude-code": claude_code_integration,
    "codex": codex_integration,
}


def probe_gateway(target: GatewayTarget, timeout: float = GATEWAY_PROBE_TIMEOUT) -> tuple[bool, str]:
    """Send an MCP tools/list to the gateway; reachable only if it answers with tools."""
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}}).encode()
    request = urllib.request.Request(target.url, data=body, headers=dict(target.headers), method="POST")
    context = None
    if target.url.startswith("https://"):
        if target.ca_path and not Path(target.ca_path).exists():
            return False, f"SCOS_TLS_CA_PATH does not exist: {target.ca_path}"
        context = ssl.create_default_context(cafile=target.ca_path or None)
    try:
        with urllib.request.urlopen(request, timeout=timeout, context=context) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return False, f"{target.url} answered HTTP {exc.code}"
    except (urllib.error.URLError, OSError) as exc:
        return False, f"{target.url} unreachable: {exc}"
    except (UnicodeDecodeError, ValueError) as exc:
        return False, f"{target.url} did not answer JSON-RPC: {exc}"
    if not isinstance(payload, dict):
        return False, f"{target.url} did not answer JSON-RPC"
    if payload.get("error"):
        return False, f"{target.url} returned an MCP error: {payload['error']}"
    tools = (payload.get("result") or {}).get("tools")
    if not isinstance(tools, list) or not tools:
        return False, f"{target.url} answered tools/list with no tools"
    return True, f"{target.url} answered tools/list with {len(tools)} tools"


def check_presence(agent_id: str, home: Optional[Path] = None) -> tuple[bool, list[str]]:
    home = home or Path.home()
    binaries, paths = PRESENCE_CHECKS.get(agent_id, ([], []))
    evidence = []
    for b in binaries:
        if shutil.which(b):
            evidence.append(f"binary:{b}")
    for p in paths:
        if _expand(p, home).exists():
            evidence.append(f"path:{p}")
    return bool(evidence), evidence


def verification_checks(agent_id: str, home: Path) -> list[Check]:
    """Run every per-machine check VERIFIED depends on, stopping at the first failure."""
    binaries, _ = PRESENCE_CHECKS.get(agent_id, ([], []))
    found = [b for b in binaries if shutil.which(b)]
    checks = [Check(
        "binary",
        bool(found),
        f"{found[0]} at {shutil.which(found[0])}" if found else f"none of {binaries} on PATH",
    )]
    if not checks[-1].passed:
        return checks
    integration = INTEGRATION_CHECKS.get(agent_id)
    if integration is None:
        checks.append(Check("integration_config", False, "no integration check defined for this agent"))
        return checks
    target, detail = integration(home)
    checks.append(Check("integration_config", target is not None, detail))
    if target is None:
        return checks
    reachable, detail = probe_gateway(target)
    checks.append(Check("mcp_gateway", reachable, detail))
    return checks


def build_report(home: Optional[Path] = None) -> dict:
    home = home or Path.home()
    registry = json.loads(REGISTRY_PATH.read_text())
    rows = []
    for entry in registry["agents"]:
        agent_id = entry["id"]
        present, evidence = check_presence(agent_id, home)
        checks: list[Check] = []
        if agent_id in LIFECYCLE_TESTED:
            checks = verification_checks(agent_id, home)
        if checks and all(c.passed for c in checks):
            tier, note = LIFECYCLE_TESTED[agent_id]
            status = "VERIFIED"
        elif present and checks:
            failed = "; ".join(f"{c.name}: {c.detail}" for c in checks if not c.passed)
            tier, note, status = "unconfirmed", f"lifecycle test exists, but on this machine {failed}", "PRESENT"
        elif present:
            tier, note, status = "unconfirmed", "registered + present, no lifecycle test run", "PRESENT"
        else:
            tier, note, status = "n/a", "no binary/config dir found on this machine", "MISSING"
        rows.append({
            "agent_id": agent_id,
            "kind": entry.get("kind", "?"),
            "path": entry.get("path", "?"),
            "status": status,
            "tier": tier,
            "note": note,
            "evidence": evidence,
            "checks": [c.as_dict() for c in checks],
        })
    return {
        "generated_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "host": home.name,
        "install_command": "python3 scripts/install_agent_plugins.py",
        "agents": rows,
        "summary": {
            "verified": sum(1 for r in rows if r["status"] == "VERIFIED"),
            "present": sum(1 for r in rows if r["status"] == "PRESENT"),
            "missing": sum(1 for r in rows if r["status"] == "MISSING"),
            "total": len(rows),
        },
    }


def print_table(report: dict) -> None:
    rows = report["agents"]
    width = max(len(r["agent_id"]) for r in rows)
    print(f"{'agent_id':<{width}}  {'kind':<16}  {'status':<9}  {'tier':<8}  note")
    print("-" * (width + 70))
    for r in rows:
        print(f"{r['agent_id']:<{width}}  {r['kind']:<16}  {r['status']:<9}  {r['tier']:<8}  {r['note']}")
    s = report["summary"]
    print(f"\n{s['verified']} verified, {s['present']} present-unconfirmed, {s['missing']} missing, "
          f"of {s['total']} registered agents.")
    if s["missing"] or s["present"]:
        print(f"\nTo install/refresh bundles for agents present on this machine, run:\n"
              f"  {report['install_command']}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--json", action="store_true", help="emit the report as JSON (for dashboard/CI consumption)")
    args = parser.parse_args()
    report = build_report()
    if args.json:
        json.dump(report, sys.stdout, indent=2)
        sys.stdout.write("\n")
    else:
        print_table(report)


if __name__ == "__main__":
    main()
