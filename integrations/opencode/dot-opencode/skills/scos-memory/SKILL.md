---
name: scos-memory
description: Use the SCOS Memory Fabric tools (scos_recall, scos_status, scos_explain, scos_user_model, scos_memory_graph, scos_correct, scos_forget) to search, inspect, and manage persistent cross-session memory. Use when the user says "recall", "what do you remember about", "check memory status", "why do you believe/remember", "forget that", or "that's wrong, it's actually".
---

OpenCode's reminder isn't a session-start hook the way Claude Code's and
Codex's are — it's `.opencode/plugins/scos-memory-reminder.ts`, a TypeScript
plugin using OpenCode's `experimental.chat.system.transform` hook. It tracks
already-reminded session IDs in memory and pushes the reminder into the
system prompt exactly once, the first time that hook fires for a given
`session.created` event. If the OpenCode process restarts mid-session, the
in-memory tracking resets and the reminder can fire again — that's expected,
not a bug to route around.

There are no slash commands here either (OpenCode's plugin API doesn't
expose one for this), so every recall, explain, or status check happens
through you calling the tool directly in response to what the user asks in
plain text.

The `scos` MCP server (registered in this project's `.opencode/mcp.json`)
is a bundled stdio bridge (`.opencode/scripts/scos-mcp-bridge.py`) that
talks directly to the SCOS Memory Fabric's `hermes-provider` on rabuntu,
reading connection details from `~/.hermes/scos.env` on this machine (see
`docs/agent-connection-guide.md` §2). It exposes seven tools, scoped to
the tenant and principal in that file.

## Tools

| Tool | Purpose | Required args |
|---|---|---|
| `scos_recall` | Search memory for relevant context | `query` (string), `max_items` (optional int) |
| `scos_status` | Check memory provider health/reachability | none |
| `scos_explain` | Show why a memory is held and its supporting evidence | `memory_id` |
| `scos_user_model` | Inspect the stored user representation | none |
| `scos_memory_graph` | Inspect memory entities and their relationships | none |
| `scos_correct` | Correct a stored memory, preserving the superseded version | `memory_id`, `value` |
| `scos_forget` | Request removal of a specific memory | `memory_id` |

## When to use each

- **Start of nontrivial work, or when the user references past context**
  ("we talked about this before", "what did we decide about X") → call
  `scos_recall` with a focused query before answering from assumption. The
  one-time system-prompt reminder only covers the first turn of a
  session — recall again whenever the topic shifts later in the same
  session.
- **User asks whether memory/the connection is working** → `scos_status`.
- **User asks "why do you think/remember that" about a specific fact** →
  `scos_explain` with that memory's id (get the id from a prior
  `scos_recall` result, don't invent one).
- **User asks what you know about them, broadly** → `scos_user_model`.
- **User asks how memories relate to each other, or wants a broader
  picture** → `scos_memory_graph`.
- **User says a stored fact is wrong** → `scos_correct`, quoting back the
  corrected value before calling, since it's a mutation.
- **User explicitly asks to forget/delete something** → `scos_forget`,
  only after confirming the specific `memory_id` and getting the user's
  explicit go-ahead — this is destructive and not easily undone.

## Anti-patterns

WRONG: skipping `scos_recall` and answering "I don't have any memory of
that" from the current conversation alone.

RIGHT: call `scos_recall` first, and only report "nothing found" after the
tool actually returns empty.

WRONG: calling `scos_forget` because the user seems annoyed about
something being remembered, without asking which specific memory.

RIGHT: ask for (or look up via `scos_recall`/`scos_memory_graph`) the
exact `memory_id`, restate what will be forgotten, and get confirmation
first.

## See also

- `docs/agent-connection-guide.md` — how the gateway/bridge is wired for
  every agent on this fleet, and where credentials live.
- `integrations/opencode/README.md` — how this integration installs.
- `integrations/claude/`, `integrations/codex/` — the equivalent
  packaging for other agents, same backend but with a hook-based
  (not system-prompt-transform-based) reminder.
