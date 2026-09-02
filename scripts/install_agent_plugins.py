#!/usr/bin/env python3
"""Install the SCOS agent bundles into the local user's agent configs.

This is deliberately additive: existing MCP servers remain untouched and a
timestamped backup is made before each changed configuration file.
"""
from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
HOME = Path.home()
BRIDGE = HOME / ".hermes" / "scripts" / "scos-mcp-bridge.py"
SCOS_ENV = HOME / ".hermes" / "scos.env"
CLAUDE_SETTINGS = HOME / ".claude" / "settings.json"
CLAUDE_PLUGIN_KEY = "scos-memory-fabric@scos-fabric-tools"
STAMP = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def server(agent: str) -> dict:
    return {
        "command": "/usr/bin/python3",
        "args": ["-E", str(BRIDGE)],
        "env": {"SCOS_AGENT_NAME": agent},
        "enabled": True,
        "description": "SCOS Memory Fabric",
    }


def backup(path: Path) -> None:
    if path.exists():
        target = path.with_name(path.name + f".bak-scos-{STAMP}")
        shutil.copy2(path, target)
        print(f"backup {target}")


def merge_json(path: Path, key: str, name: str, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = {}
    if path.exists():
        data = json.loads(path.read_text())
    section = data.setdefault(key, {})
    section[name] = value
    backup(path)
    path.write_text(json.dumps(data, indent=2) + "\n")
    print(f"installed {name} -> {path}")


def copy_rule(source: Path, target: Path) -> None:
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, target)
    print(f"installed rules -> {target}")


def copy_commands(source_dir: Path, target_dir: Path) -> None:
    """Copy each file in source_dir into target_dir, backing up collisions."""
    target_dir.mkdir(parents=True, exist_ok=True)
    for source in source_dir.iterdir():
        if not source.is_file():
            continue
        target = target_dir / source.name
        backup(target)
        shutil.copy2(source, target)
        print(f"installed command -> {target}")


def run(command: list[str]) -> None:
    print("$", " ".join(command))
    subprocess.run(command, cwd=ROOT, check=True)


def load_scos_env() -> dict:
    env = {}
    if SCOS_ENV.exists():
        for line in SCOS_ENV.read_text().splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            env[key] = value
    return env


def configure_claude_plugin_options() -> None:
    """Pre-fill the Claude Code plugin's userConfig from ~/.hermes/scos.env so
    `claude plugin install` doesn't have to interactively prompt for four raw
    values the user has no reason to know off-hand. Only fills options that
    aren't already set, so a value someone deliberately overrode by hand is
    left alone."""
    env = load_scos_env()
    fields = {
        "gateway_url": env.get("SCOS_GATEWAY_URL") or env.get("SCOS_MCP_GATEWAY_URL"),
        "bearer_token": env.get("SCOS_BEARER_TOKEN") or env.get("SCOS_HERMES_BEARER_TOKEN"),
        "tenant_id": env.get("SCOS_TENANT_ID"),
        "principal_id": env.get("SCOS_PRINCIPAL_ID"),
    }
    if not any(fields.values()):
        print(f"skipped plugin config prefill: {SCOS_ENV} not found or empty")
        return

    data = {}
    if CLAUDE_SETTINGS.exists():
        data = json.loads(CLAUDE_SETTINGS.read_text())
    plugin_configs = data.setdefault("pluginConfigs", {})
    options = plugin_configs.setdefault(CLAUDE_PLUGIN_KEY, {}).setdefault("options", {})

    filled = []
    for key, value in fields.items():
        if value and not options.get(key):
            options[key] = value
            filled.append(key)

    if not filled:
        print(f"plugin config already set for {CLAUDE_PLUGIN_KEY}, nothing to prefill")
        return

    backup(CLAUDE_SETTINGS)
    CLAUDE_SETTINGS.parent.mkdir(parents=True, exist_ok=True)
    CLAUDE_SETTINGS.write_text(json.dumps(data, indent=2) + "\n")
    print(f"prefilled plugin config ({', '.join(filled)}) -> {CLAUDE_SETTINGS}")


def run_if_installed(command: list[str]) -> None:
    """Run an optional native installer only when that agent is present."""
    if shutil.which(command[0]) is None:
        print(f"skipped unavailable agent CLI: {command[0]}")
        return
    run(command)


def install(skip_gemini: bool = False) -> None:
    BRIDGE.parent.mkdir(parents=True, exist_ok=True)
    backup(BRIDGE)
    shutil.copy2(ROOT / "integrations/agent-plugins/scos-mcp-bridge.py", BRIDGE)
    print(f"installed shared bridge -> {BRIDGE}")

    hermes_target = HOME / ".hermes" / "plugins" / "scos-memory"
    hermes_target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copytree(ROOT / "integrations/hermes/plugins/scos-memory", hermes_target, dirs_exist_ok=True)
    print(f"installed Hermes plugin -> {hermes_target}")

    if not skip_gemini:
        run(["gemini", "extensions", "link", str(ROOT / "integrations/agent-plugins/gemini")])

    # MCP-capable agents: preserve each host's native config shape.
    merge_json(HOME / ".cline" / "mcp.json", "mcpServers", "scos", server("cline"))
    merge_json(HOME / ".cline" / "data/settings/cline_mcp_settings.json", "mcpServers", "scos", server("cline"))
    merge_json(HOME / ".cursor" / "mcp.json", "mcpServers", "scos", server("cursor"))
    for path in [HOME / ".codeium/windsurf/mcp_config.json", HOME / ".windsurf/mcp.json"]:
        merge_json(path, "mcpServers", "scos", server("windsurf-devin"))
    merge_json(HOME / ".config/Devin/User/mcp.json", "servers", "scos", server("windsurf-devin"))
    for path in [HOME / ".gemini/antigravity/mcp_config.json", HOME / ".gemini/config/mcp_config.json"]:
        merge_json(path, "mcpServers", "scos", server("antigravity"))
    merge_json(HOME / ".qwen/settings.json", "mcpServers", "scos", server("qwen"))
    merge_json(HOME / ".openclaw/mcp.json", "mcpServers", "scos", server("openclaw"))
    merge_json(HOME / ".opencode/mcp.json", "mcpServers", "scos", server("opencode"))
    merge_json(HOME / ".config/opencode/opencode.json", "mcp", "scos", {
        "type": "local", "command": ["/usr/bin/python3", str(BRIDGE)],
        "environment": {"SCOS_AGENT_NAME": "opencode"},
    })

    continue_mcp = HOME / ".continue/mcpServers/scos.json"
    continue_mcp.parent.mkdir(parents=True, exist_ok=True)
    backup(continue_mcp)
    continue_mcp.write_text(json.dumps({"mcpServers": {"scos": server("continue")}}, indent=2) + "\n")
    print(f"installed scos -> {continue_mcp}")

    # Kilo already uses the repository's native config shape; this is additive.
    merge_json(HOME / ".config/kilo/kilo.json", "mcp", "scos", {
        "type": "local", "command": ["/usr/bin/python3", str(BRIDGE)],
        "environment": {"SCOS_AGENT_NAME": "kilo"},
    })

    copy_rule(ROOT / "integrations/agent-plugins/cline/CLINE.md", HOME / ".clinerules/scos-memory.md")
    copy_rule(ROOT / "integrations/agent-plugins/cursor/CURSOR.md", HOME / ".cursor/rules/scos-memory.mdc")
    copy_rule(ROOT / "integrations/agent-plugins/windsurf/WINDSURF.md", HOME / ".windsurf/rules/scos-memory.md")
    copy_rule(ROOT / "integrations/agent-plugins/antigravity/ANTIGRAVITY.md", HOME / ".gemini/antigravity/scos-memory.md")
    copy_rule(ROOT / "integrations/agent-plugins/continue/CONTINUE.md", HOME / ".continue/rules/scos-memory.md")
    copy_rule(ROOT / "integrations/agent-plugins/aider/AIDER.md", HOME / ".aider.scos.md")

    # Slash commands. Codex, Continue, Kilo, and Qwen Code resolve custom
    # commands from a user-global directory rather than the plugin/rules
    # bundle itself, so they need an explicit copy here; Cline, Cursor,
    # Windsurf, and Antigravity resolve commands/workflows per-project and
    # are meant to be checked into each repo instead (see their READMEs).
    copy_commands(ROOT / "integrations/codex/plugins/scos-memory-fabric/prompts", HOME / ".codex" / "prompts")
    copy_commands(ROOT / "integrations/agent-plugins/continue/prompts", HOME / ".continue" / "prompts")
    copy_commands(ROOT / "integrations/kilo/commands", HOME / ".kilo" / "commands")
    copy_commands(ROOT / "integrations/qwen/plugin/commands", HOME / ".qwen" / "commands")

    # OpenClaw skills are each a directory (SKILL.md plus any assets), not a
    # single file, so they need copytree rather than copy_commands.
    openclaw_skills_target = HOME / ".openclaw" / "skills"
    openclaw_skills_target.mkdir(parents=True, exist_ok=True)
    for skill_dir in (ROOT / "integrations/openclaw/skills").iterdir():
        if skill_dir.is_dir():
            shutil.copytree(skill_dir, openclaw_skills_target / skill_dir.name, dirs_exist_ok=True)
            print(f"installed skill -> {openclaw_skills_target / skill_dir.name}")

    run_if_installed(["codex", "plugin", "add", "scos-memory-fabric@scos-fabric-tools"])
    run_if_installed(["claude", "plugin", "marketplace", "add", str(ROOT / "integrations/claude")])
    configure_claude_plugin_options()
    run_if_installed(["claude", "plugin", "install", "scos-memory-fabric@scos-fabric-tools"])


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--skip-gemini", action="store_true", help="skip Gemini after it has already been linked")
    args = parser.parse_args()
    install(skip_gemini=args.skip_gemini)


if __name__ == "__main__":
    main()
