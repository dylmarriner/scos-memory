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

Each completed turn is written as a recallable memory item (memory_write)
and journaled as a session turn (sync_turn); see the Claude Code variant's
docstring. Failures are logged to ~/.hermes/auto-persist.log.
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


def log(msg):
    try:
        with open(LOG_PATH, "a") as f:
            f.write(f"{time.strftime('%Y-%m-%dT%H:%M:%S')} {msg}\n")
    except OSError:
        pass


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

    env = load_persist_env()
    if env is None:
        return
    persist_turn(env, "codex-stop-hook", "codex-turn",
                 payload.get("session_id", "unknown"), user_text, assistant_text)


if __name__ == "__main__":
    main()
