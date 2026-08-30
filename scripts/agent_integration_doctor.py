#!/usr/bin/env python3
"""Report the real (runtime-verified where possible) integration status of
every agent listed in integrations/agent-plugins/registry.json.

GOVERNANCE.md 4.20/4.17 requires truthful, evidence-backed status - this
deliberately does not infer "working" from a file existing. It distinguishes
three states per agent:

  VERIFIED   - a live memory-blind round trip (separate process, a fact
               stated with no memory/SCOS wording, recalled in a second
               separate process) has actually been run and passed. See
               docs/agent-connection-guide.md for how each was run.
  PRESENT    - the registry claims an integration and the machine shows
               real evidence (binary/config dir), but no automatic-lifecycle
               test has been run - tier is unconfirmed, not assumed Tier A.
  MISSING    - registry claims an integration but no local evidence found.

Extend the VERIFIED_AGENTS dict below only after actually running the
memory-blind test for that agent id - do not mark an agent verified because
its hooks.json merely exists.
"""
from __future__ import annotations

import json
import shutil
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
REGISTRY_PATH = ROOT / "integrations" / "agent-plugins" / "registry.json"

# agent_id -> (tier, evidence note). Populate only from an actual test run.
VERIFIED_AGENTS = {
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


def check_presence(agent_id: str) -> tuple[bool, list[str]]:
    binaries, paths = PRESENCE_CHECKS.get(agent_id, ([], []))
    evidence = []
    for b in binaries:
        if shutil.which(b):
            evidence.append(f"binary:{b}")
    for p in paths:
        expanded = Path(p).expanduser()
        if expanded.exists():
            evidence.append(f"path:{p}")
    return bool(evidence), evidence


def main() -> None:
    registry = json.loads(REGISTRY_PATH.read_text())
    rows = []
    for entry in registry["agents"]:
        agent_id = entry["id"]
        present, evidence = check_presence(agent_id)
        if agent_id in VERIFIED_AGENTS:
            tier, note = VERIFIED_AGENTS[agent_id]
            status = "VERIFIED"
        elif present:
            tier, note, status = "unconfirmed", "registered + present, no lifecycle test run", "PRESENT"
        else:
            tier, note, status = "n/a", "no binary/config dir found on this machine", "MISSING"
        rows.append((agent_id, entry.get("kind", "?"), status, tier, note, evidence))

    width = max(len(r[0]) for r in rows)
    print(f"{'agent_id':<{width}}  {'kind':<16}  {'status':<9}  {'tier':<8}  note")
    print("-" * (width + 70))
    for agent_id, kind, status, tier, note, evidence in rows:
        print(f"{agent_id:<{width}}  {kind:<16}  {status:<9}  {tier:<8}  {note}")

    verified = sum(1 for r in rows if r[2] == "VERIFIED")
    present = sum(1 for r in rows if r[2] == "PRESENT")
    missing = sum(1 for r in rows if r[2] == "MISSING")
    print(f"\n{verified} verified, {present} present-unconfirmed, {missing} missing, "
          f"of {len(rows)} registered agents.")


if __name__ == "__main__":
    main()
