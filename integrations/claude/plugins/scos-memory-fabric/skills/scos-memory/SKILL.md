---
name: scos-memory
description: Use the SCOS Memory Fabric's mcp-gateway tools (scos_recall, scos_status, scos_explain, scos_user_model, scos_memory_graph, scos_correct, scos_forget) to search, inspect, and manage persistent cross-session memory. Use when the user says "recall", "what do you remember about", "check memory status", "why do you believe/remember", "forget that", or "that's wrong, it's actually".
user-invocable: false
---

The `scos` MCP server (this plugin) connects to the SCOS Memory Fabric's
`mcp-gateway` (`services/mcp-gateway`), which proxies to `hermes-provider`
and `memory-intelligence`. It exposes seven tools, scoped to the tenant and
principal configured at plugin install time.

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
  `scos_recall` with a focused query before answering from assumption.
- **User asks whether memory/the connection is working** → `scos_status`.
- **User asks "why do you think/remember that" about a specific fact** →
  `scos_explain` with that memory's id (get the id from a prior `scos_recall`
  result, don't invent one).
- **User asks what you know about them, broadly** → `scos_user_model`.
- **User asks how memories relate to each other, or wants a broader picture**
  → `scos_memory_graph`.
- **User says a stored fact is wrong** → `scos_correct`, quoting back the
  corrected value before calling, since it's a mutation.
- **User explicitly asks to forget/delete something** → `scos_forget`, only
  after confirming the specific `memory_id` and getting the user's explicit
  go-ahead — this is destructive and not easily undone.

## Caution: `scos_correct` and `scos_forget`

As of the current `mcp-gateway` build, these two tools are wired into the
tool list but have **no backing implementation** on this fleet
(`services/mcp-gateway/cmd/server/main.go` routes them to a stub). Calling
them may return an error or a no-op rather than actually mutating memory.
Tell the user this if a call doesn't behave as expected — don't assume
silence means success.

## Anti-patterns

WRONG: skipping `scos_recall` and answering "I don't have any memory of
that" from the current conversation alone.

RIGHT: call `scos_recall` first, and only report "nothing found" after the
tool actually returns empty.

WRONG: calling `scos_forget` because the user seems annoyed about something
being remembered, without asking which specific memory.

RIGHT: ask for (or look up via `scos_recall`/`scos_memory_graph`) the exact
`memory_id`, restate what will be forgotten, and get confirmation first.

## See also

- `docs/agent-connection-guide.md` — how the gateway is wired for every
  agent on this fleet, and where credentials live.
- `integrations/claude/README.md` — how this plugin and the Desktop
  extension both point at the same backend.
