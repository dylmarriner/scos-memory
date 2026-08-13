#!/usr/bin/env python3
"""Portable launcher for the machine-level SCOS MCP bridge."""
import os
import runpy
from pathlib import Path


bridge = Path(os.environ.get("SCOS_MCP_BRIDGE", "~/.hermes/scripts/scos-mcp-bridge.py")).expanduser()
if not bridge.is_file():
    raise SystemExit(f"SCOS bridge not found: {bridge}")
runpy.run_path(str(bridge), run_name="__main__")
