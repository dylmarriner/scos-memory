#!/usr/bin/env python3
"""Stop hook: persists the completed turn's last exchange to SCOS via
hermes-provider's memory_write, with no explicit write tool call by the
model (GOVERNANCE.md 4.14 - platform-owned persistence, not agent-optional).

Each completed turn is written twice, both idempotently: as a recallable
memory item (memory_write, keyed on a content digest so a repeat exchange
versions the same row) and as a journaled session turn (sync_turn, with the
Stop payload's session_id mapped to the provider's UUID contract the same way
the Hermes plugin maps it), so the turn enters the same session/turn journal
the consolidation workers read.

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
import uuid

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


def ssl_context(env):
    # Same policy as the Hermes plugin and MCP bridges: verify by default,
    # optional private CA, explicit opt-in to skip verification.
    ca_path = env.get("SCOS_TLS_CA_PATH", "")
    if ca_path and not os.path.exists(ca_path):
        raise RuntimeError(f"SCOS_TLS_CA_PATH does not exist: {ca_path}")
    ctx = ssl.create_default_context(cafile=ca_path or None)
    if env.get("SCOS_TLS_INSECURE_SKIP_VERIFY") == "1":
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    return ctx


def stable_session_uuid(session_id, env):
    # hermes-provider requires a UUID session_id; derive it exactly as the
    # Hermes plugin does so one agent session maps to one SCOS session.
    try:
        return str(uuid.UUID(session_id))
    except (ValueError, AttributeError, TypeError):
        identity = f"{env.get('SCOS_TENANT_ID', '')}\0{env.get('SCOS_PRINCIPAL_ID', '')}\0{session_id}"
        return str(uuid.uuid5(uuid.NAMESPACE_URL, "scos-hermes-session:" + identity))


def post_json(env, path, payload):
    headers = {
        "Authorization": f"Bearer {env['SCOS_HERMES_BEARER_TOKEN']}",
        "Content-Type": "application/json",
    }
    if env.get("SCOS_TENANT_ID"):
        headers["X-SCOS-Tenant-ID"] = env["SCOS_TENANT_ID"]
    if env.get("SCOS_PRINCIPAL_ID"):
        headers["X-SCOS-Principal-ID"] = env["SCOS_PRINCIPAL_ID"]
    req = urllib.request.Request(
        env["SCOS_HERMES_PROVIDER_URL"].rstrip("/") + path,
        data=json.dumps(payload).encode(), method="POST", headers=headers,
    )
    with urllib.request.urlopen(req, timeout=8, context=ssl_context(env)) as resp:
        resp.read()


def post_with_retry(env, path, payload):
    for attempt in (1, 2):
        try:
            post_json(env, path, payload)
            return True
        except urllib.error.HTTPError as e:
            # The server processed and rejected the request (auth,
            # validation, RLS); an identical retry fails the same way.
            log(f"{path} failed: HTTP {e.code} {e.read().decode('utf-8', 'replace')[:500]}")
            return False
        except Exception as e:
            if attempt == 2:
                log(f"{path} failed after retry: {type(e).__name__}: {e}")
                return False
            time.sleep(0.5)
    return False


def persist_turn(env, source, key_prefix, session_id, user_text, assistant_text):
    """Write the exchange as a recallable memory item and journal it as a
    session turn. Both writes are idempotent: the memory key and turn_id are
    content-derived, so a re-fired hook versions/dedupes instead of
    duplicating."""
    namespace_id = env["SCOS_NAMESPACE_ID"]
    user_text, assistant_text = user_text[:8000], assistant_text[:8000]
    post_with_retry(env, "/v1/provider/memory_write", {
        "namespace_id": namespace_id,
        "key": stable_key(key_prefix, session_id, user_text),
        "value": f"User: {user_text}\n\nAssistant: {assistant_text}",
        "scope": "agent",
        "source": source,
    })
    post_with_retry(env, "/v1/provider/sync_turn", {
        "namespace_id": namespace_id,
        "session_id": stable_session_uuid(session_id, env),
        "turn_id": hashlib.sha256(f"{session_id}\0{user_text}\0{assistant_text}".encode("utf-8", "replace")).hexdigest(),
        "messages": [
            {"role": "user", "content": user_text},
            {"role": "assistant", "content": assistant_text},
        ],
        "tool_calls": [],
    })


def stable_key(prefix, session_id, user_text):
    # hashlib, not hash(): Python's hash() is salted per process, which gave
    # the same exchange a different key on every hook run and defeated the
    # (tenant, namespace, key) upsert/versioning.
    digest = hashlib.sha256(user_text.encode("utf-8", "replace")).hexdigest()[:16]
    return f"{prefix}:{session_id}:{digest}"


def load_persist_env():
    env = load_env()
    missing = [k for k in ("SCOS_HERMES_PROVIDER_URL", "SCOS_HERMES_BEARER_TOKEN", "SCOS_NAMESPACE_ID") if not env.get(k)]
    if missing:
        log(f"skipped: missing {', '.join(missing)} in {ENV_PATH}")
        return None
    return env


def end_session(payload):
    """SessionEnd: close the provider session so hermes-provider drains its
    queued work and raises on_session_end for this session."""
    env = load_persist_env()
    if env is None:
        return
    post_with_retry(env, "/v1/provider/session_end", {
        "session_id": stable_session_uuid(payload.get("session_id", "unknown"), env),
    })


def main():
    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, ValueError):
        return
    if len(sys.argv) > 1 and sys.argv[1] == "session-end":
        end_session(payload)
        return
    transcript_path = payload.get("transcript_path")
    if not transcript_path:
        return

    user_text, assistant_text = last_exchange(transcript_path)
    if not user_text or not assistant_text:
        return

    env = load_persist_env()
    if env is None:
        return
    persist_turn(env, "claude-code-stop-hook", "claude-code-turn",
                 payload.get("session_id", "unknown"), user_text, assistant_text)


if __name__ == "__main__":
    main()
