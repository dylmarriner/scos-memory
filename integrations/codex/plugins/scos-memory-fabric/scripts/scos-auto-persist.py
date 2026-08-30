#!/usr/bin/env python3
"""Stop hook (Codex variant): persists the completed turn's last exchange to
SCOS via hermes-provider's memory_write, with no explicit write tool call by
the model (GOVERNANCE.md 4.14 - platform-owned persistence, not agent-
optional).

Codex's Stop hook payload already includes `last_assistant_message` directly,
so only the user side needs pulling from the transcript. Codex's transcript
("rollout") format differs from Claude Code's: entries are
{"payload": {"type": "message", "role": ..., "content": [{"type":
"input_text"|"output_text", "text": ...}]}}, and system-injected wrapper
messages (plugin banners, skill lists, our own auto-recall injection) share
role "user"/"developer" with the real prompt - they're filtered out by
skipping content that starts with "<" (tag-wrapped) or matches a known
injected-marker prefix.

This is a bounded, deliberately simple persistence path (last exchange from
one transcript), not the full Turn Coordinator/journal lifecycle in
GOVERNANCE.md 11 - see the Claude Code variant's docstring for the same
caveat.
"""
import json
import os
import ssl
import sys
import urllib.request

ENV_PATH = os.path.expanduser("~/.hermes/scos.env")
INJECTED_MARKERS = ("[scos-auto-recall]", "<recommended_plugins>", "<skills_instructions>")


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


def text_of(content):
    if not isinstance(content, list):
        return ""
    return " ".join(b.get("text", "") for b in content if isinstance(b, dict) and "text" in b)


def last_real_user_message(transcript_path):
    try:
        with open(transcript_path) as f:
            lines = [json.loads(l) for l in f if l.strip()]
    except (OSError, json.JSONDecodeError):
        return None

    for entry in reversed(lines):
        payload = entry.get("payload", {})
        if payload.get("type") != "message" or payload.get("role") != "user":
            continue
        text = text_of(payload.get("content"))
        if not text.strip() or text.lstrip().startswith("<"):
            continue
        if any(text.startswith(m) for m in INJECTED_MARKERS):
            continue
        return text
    return None


def main():
    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, ValueError):
        return

    assistant_text = payload.get("last_assistant_message", "")
    transcript_path = payload.get("transcript_path")
    if not assistant_text or not transcript_path:
        return

    user_text = last_real_user_message(transcript_path)
    if not user_text:
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
        "key": f"codex-turn:{session_id}:{hash(user_text) & 0xffffffff}",
        "value": f"User: {user_text[:2000]}\n\nAssistant: {assistant_text[:2000]}",
        "scope": "agent",
        "source": "codex-stop-hook",
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
