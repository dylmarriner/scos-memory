#!/usr/bin/env python3
"""Launch the shared SCOS stdio bridge for the Qwen extension."""
import os
import sys

bridge = os.path.expanduser(os.environ.get("SCOS_MCP_BRIDGE", "~/.hermes/scripts/scos-mcp-bridge.py"))
if not os.path.isfile(bridge):
    raise SystemExit(f"SCOS bridge not found: {bridge}")
os.execv(sys.executable, [sys.executable, "-E", bridge])
