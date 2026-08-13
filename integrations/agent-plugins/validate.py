#!/usr/bin/env python3
"""Validate the agent plugin catalog without contacting the memory service."""
import json
from pathlib import Path


ROOT = Path(__file__).parent
REGISTRY = ROOT / "registry.json"


def main():
    registry = json.loads(REGISTRY.read_text())
    seen = set()
    for agent in registry["agents"]:
        agent_id = agent["id"]
        if agent_id in seen:
            raise SystemExit(f"duplicate agent id: {agent_id}")
        seen.add(agent_id)
        if not agent["agent_name"] or agent["agent_name"].startswith("~"):
            raise SystemExit(f"invalid agent name: {agent_id}")
        path = (ROOT / agent["path"]).resolve()
        if not path.exists():
            raise SystemExit(f"missing bundle for {agent_id}: {path}")
        if agent["kind"] != "native-plugin" and not (path / "README.md").exists():
            raise SystemExit(f"missing README for {agent_id}: {path}")
    print(f"validated {len(seen)} SCOS agent bundles")


if __name__ == "__main__":
    main()
