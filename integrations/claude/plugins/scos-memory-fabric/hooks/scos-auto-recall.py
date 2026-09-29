#!/usr/bin/env python3
"""UserPromptSubmit hook: calls scos_recall automatically, before the model
ever sees the prompt, so retrieval does not depend on the model choosing to
call a tool (GOVERNANCE.md 4.14/21.4 - platform-owned pre-turn retrieval).
"""
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request

ENV_PATH = os.path.expanduser("~/.hermes/scos.env")
LOG_PATH = os.path.expanduser("~/.hermes/auto-recall.log")


def log(msg):
    try:
        with open(LOG_PATH, "a") as f:
            f.write(f"{time.strftime('%Y-%m-%dT%H:%M:%S')} {msg}\n")
    except OSError:
        pass


def ssl_context(env):
    # Same policy as scos-auto-persist and the MCP bridges: verify by
    # default, optional private CA, explicit opt-in to skip verification.
    ca_path = env.get("SCOS_TLS_CA_PATH", "")
    if ca_path and not os.path.exists(ca_path):
        raise RuntimeError(f"SCOS_TLS_CA_PATH does not exist: {ca_path}")
    ctx = ssl.create_default_context(cafile=ca_path or None)
    if env.get("SCOS_TLS_INSECURE_SKIP_VERIFY") == "1":
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    return ctx


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
    # Recall must never block the prompt, but a failure is logged rather than
    # swallowed so a broken gateway or credential is visible.
    try:
        with urllib.request.urlopen(req, timeout=8, context=ssl_context(env)) as resp:
            result = json.load(resp)
    except urllib.error.HTTPError as e:
        log(f"scos_recall failed: HTTP {e.code} {e.read().decode('utf-8', 'replace')[:500]}")
        return
    except Exception as e:
        log(f"scos_recall failed: {type(e).__name__}: {e}")
        return
    if result.get("error"):
        log(f"scos_recall returned an error: {json.dumps(result['error'])[:500]}")
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
