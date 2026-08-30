#!/usr/bin/env python3
"""Stop hook: persists the completed turn's last exchange to SCOS via
hermes-provider's memory_write, with no explicit write tool call by the
model (GOVERNANCE.md 4.14 - platform-owned persistence, not agent-optional).

This is a bounded, deliberately simple persistence path (last user+assistant
message pair from the transcript), not the full Turn Coordinator/journal
lifecycle described in GOVERNANCE.md 11 - that requires session/turn IDs
this hook doesn't have. It closes the "nothing is ever saved automatically"
gap for Claude Code; it is not a substitute for a real control-plane ingress
adapter.
"""
import json
import os
import ssl
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


def last_exchange(transcript_path):
    """Return (user_text, assistant_text) for the last turn in a Claude Code
    transcript (JSONL of message objects), or (None, None) if unavailable."""
    try:
        with open(transcript_path) as f:
            lines = [json.loads(l) for l in f if l.strip()]
    except (OSError, json.JSONDecodeError):
        return None, None

    def text_of(entry):
        msg = entry.get("message", {})
        content = msg.get("content", "")
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            return " ".join(
                b.get("text", "") for b in content if isinstance(b, dict) and b.get("type") == "text"
            )
        return ""

    user_text = assistant_text = None
    for entry in reversed(lines):
        role = entry.get("message", {}).get("role")
        if role == "assistant" and assistant_text is None:
            assistant_text = text_of(entry)
        elif role == "user" and assistant_text is not None and user_text is None:
            user_text = text_of(entry)
            break
    return user_text, assistant_text


def main():
    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, ValueError):
        return
    transcript_path = payload.get("transcript_path")
    if not transcript_path:
        return

    user_text, assistant_text = last_exchange(transcript_path)
    if not user_text or not assistant_text:
        return

    env = load_env()
    base_url = env.get("SCOS_HERMES_PROVIDER_URL")
    token = env.get("SCOS_HERMES_BEARER_TOKEN")
    namespace_id = env.get("SCOS_NAMESPACE_ID")
    if not base_url or not token or not namespace_id:
        return

    session_id = payload.get("session_id", "unknown")
    body = json.dumps({
        "namespace_id": namespace_id,
        "key": f"claude-code-turn:{session_id}:{hash(user_text) & 0xffffffff}",
        "value": f"User: {user_text[:2000]}\n\nAssistant: {assistant_text[:2000]}",
        "scope": "agent",
        "source": "claude-code-stop-hook",
    }).encode()

    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    req = urllib.request.Request(
        base_url.rstrip("/") + "/v1/provider/memory_write",
        data=body, method="POST",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    try:
        urllib.request.urlopen(req, timeout=8, context=ctx)
    except Exception:
        return


if __name__ == "__main__":
    main()
