#!/usr/bin/env python3
"""Launch the shared SCOS stdio bridge for the Qwen extension."""
import os
import subprocess
import sys

bridge = os.path.expanduser("~/.hermes/scripts/scos-mcp-bridge.py")
os.execv(sys.executable, [sys.executable, "-E", bridge])
