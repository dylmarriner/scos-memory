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

Failures are logged to ~/.hermes/auto-persist.log rather than swallowed: a
prior version of this script silently discarded every error, which is how a
500 from a missing RLS policy on memory_versions (fixed in
database/policies/000013_memory_versions_write.sql) went unnoticed for as
long as it did. A write is retried once on a transient failure; a non-2xx
HTTP response is not retried (retrying an unchanged request against a
policy/auth error just fails the same way twice).
"""
import hashlib
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request

ENV_PATH = os.path.expanduser("~/.hermes/scos.env")
LOG_PATH = os.path.expanduser("~/.hermes/auto-persist.log")


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


def log(msg):
    try:
        with open(LOG_PATH, "a") as f:
            f.write(f"{time.strftime('%Y-%m-%dT%H:%M:%S')} {msg}\n")
    except OSError:
        pass


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


def stable_key(session_id, user_text):
    # Python's hash() is salted per-process (PYTHONHASHSEED) unless
    # explicitly disabled, so the previous `hash(user_text) & 0xffffffff`
    # produced a different key for the identical exchange across separate
    # invocations of this script -- defeating the upsert/versioning
    # upsertMemory keys on (tenant, namespace, hermes_key) to detect. A
    # stable digest lets a genuine repeat of the same exchange collapse into
    # a new version of the same memory_items row instead of a duplicate.
    digest = hashlib.sha256(user_text.encode("utf-8", "replace")).hexdigest()[:16]
    return f"claude-code-turn:{session_id}:{digest}"


def post_memory_write(base_url, token, body):
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    req = urllib.request.Request(
        base_url.rstrip("/") + "/v1/provider/memory_write",
        data=body, method="POST",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    return urllib.request.urlopen(req, timeout=8, context=ctx)


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
        log("skipped: missing SCOS_HERMES_PROVIDER_URL/SCOS_HERMES_BEARER_TOKEN/SCOS_NAMESPACE_ID in scos.env")
        return

    session_id = payload.get("session_id", "unknown")
    body = json.dumps({
        "namespace_id": namespace_id,
        "key": stable_key(session_id, user_text),
        "value": f"User: {user_text[:8000]}\n\nAssistant: {assistant_text[:8000]}",
        "scope": "agent",
        "source": "claude-code-stop-hook",
    }).encode()

    for attempt in (1, 2):
        try:
            post_memory_write(base_url, token, body)
            return
        except urllib.error.HTTPError as e:
            # A non-2xx response means the server processed and rejected the
            # request (auth, validation, RLS) -- retrying the same body
            # won't change that outcome, so log once and stop.
            log(f"memory_write failed: HTTP {e.code} {e.read().decode('utf-8', 'replace')[:500]}")
            return
        except Exception as e:
            if attempt == 2:
                log(f"memory_write failed after retry: {type(e).__name__}: {e}")
                return
            time.sleep(0.5)


if __name__ == "__main__":
    main()
