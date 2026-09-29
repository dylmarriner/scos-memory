#!/usr/bin/env python3
"""PostToolUse hook for scos_* / mcp__scos* tool calls.

When a call to the SCOS gateway fails, agents otherwise have no way to know
this is a *known* class of error (bad repo_path, unregistered repository_id,
missing control-plane token, ...) short of re-deriving it from scratch or
writing yet another ad hoc ".learnings" entry. This hook pattern-matches the
failure against docs/agent-connection-guide.md section 5 and .learnings/ERRORS.md,
and injects the fix directly back into context so the agent can retry correctly
instead of giving up and writing a doc.

Stdin: the PostToolUse JSON payload Claude Code sends (tool_name, tool_input,
tool_response, ...). Stdout: hookSpecificOutput.additionalContext, or nothing
if the call looks like it succeeded.
"""
import json
import sys

# (substring to match in the failure text, guidance to inject)
KNOWN_ERRORS = [
    ("GRAPHIFY_ALLOWED_ROOT",
     "Known issue (.learnings/ERRORS.md ERR-20260830-001, unresolved): the "
     "graphify provider rejects an explicit repo_path because "
     "GRAPHIFY_ALLOWED_ROOT isn't configured server-side. Workaround: omit "
     "repo_path and let it use the provider's configured default repo "
     "(GRAPHIFY_REPO_PATH). This is a server config gap, not something a "
     "client can fix by retrying with different arguments."),
    ("Ambiguous", "graphify explain hit two nodes with the same short name in "
     "different files (ERR-20260830-002). Retry with the repo-relative path "
     "or full node ID instead of the bare symbol name."),
    ("repository_id",
     "scos_code_index / scos_code_query 404s if the repo isn't registered "
     "yet. Call scos_code_repo_register first to get a repository_id, then "
     "retry."),
    ("API key required", "SCOS_BEARER_TOKEN is missing/empty in "
     "~/.hermes/scos.env on this machine."),
    ("invalid API key", "The API key is wrong or revoked -- issue a fresh one "
     "via /api/keys and update ~/.hermes/scos.env."),
    ("gateway unavailable", "Not a credentials problem -- cloud-control-plane's "
     "internal proxy hop to mcp-gateway is broken (infra issue, see "
     "docs/agent-connection-guide.md section 5)."),
    ("memory_id is required", "scos_correct/scos_forget need the exact "
     "memory_id from scos_recall or scos_explain -- confirm it with the "
     "user before retrying."),
]

FALLBACK = (
    "This scos_*/graphify_*/code_* call failed. Before writing a new "
    ".learnings entry or giving up, check docs/agent-connection-guide.md "
    "section 5 (Troubleshooting table) -- most SCOS gateway failures are "
    "already documented there or in .learnings/ERRORS.md."
)


def response_text(tool_response):
    if isinstance(tool_response, str):
        return tool_response
    if isinstance(tool_response, dict):
        return json.dumps(tool_response)
    return str(tool_response)


def looks_like_failure(payload, text):
    if isinstance(payload.get("tool_response"), dict) and payload["tool_response"].get("is_error"):
        return True
    lowered = text.lower()
    return any(tok in lowered for tok in ("error", "failed", "refus", "400", "401", "403", "404", "500"))


def main():
    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, ValueError):
        return
    tool_name = payload.get("tool_name", "")
    if "scos" not in tool_name and "graphify" not in tool_name:
        return
    text = response_text(payload.get("tool_response", ""))
    if not looks_like_failure(payload, text):
        return

    guidance = FALLBACK
    for needle, hint in KNOWN_ERRORS:
        if needle.lower() in text.lower():
            guidance = hint
            break

    print(json.dumps({
        "hookSpecificOutput": {
            "hookEventName": "PostToolUse",
            "additionalContext": f"[scos-tool-guidance] {tool_name} failed. {guidance}",
        }
    }))


if __name__ == "__main__":
    main()
