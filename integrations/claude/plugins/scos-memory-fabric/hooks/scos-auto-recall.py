#!/usr/bin/env python3
"""UserPromptSubmit hook: calls scos_recall automatically, before the model
ever sees the prompt, so retrieval does not depend on the model choosing to
call a tool (GOVERNANCE.md 4.14/21.4 - platform-owned pre-turn retrieval).
"""
import json
import os
import sys
import urllib.request

ENV_PATH = os.path.expanduser("~/.hermes/scos.env")


def load_env():
    env = {}
    try:
        with open(ENV_PATH) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                env[k] = v
    except OSError:
        pass
    return env


def main():
    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, ValueError):
        return
    prompt = payload.get("prompt", "")
    if not prompt.strip():
        return

    env = load_env()
    url = env.get("SCOS_GATEWAY_URL")
    token = env.get("SCOS_BEARER_TOKEN")
    if not url or not token:
        return

    body = json.dumps({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "scos_recall", "arguments": {"query": prompt[:500]}},
    }).encode()
    req = urllib.request.Request(url, data=body, method="POST", headers={
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
    })
    try:
        with urllib.request.urlopen(req, timeout=8) as resp:
            result = json.load(resp)
    except Exception:
        return

    content = result.get("result", {}).get("content")
    if not content:
        return
    text = content[0].get("text", "") if isinstance(content, list) else str(content)
    if not text.strip():
        return

    print(json.dumps({
        "hookSpecificOutput": {
            "hookEventName": "UserPromptSubmit",
            "additionalContext": f"[scos-auto-recall] Retrieved without an explicit tool call:\n{text[:4000]}",
        }
    }))


if __name__ == "__main__":
    main()
