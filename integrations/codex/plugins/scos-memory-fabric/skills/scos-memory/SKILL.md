---
name: scos-memory
description: Use the SCOS Memory Fabric tools (scos_recall, scos_status, scos_explain, scos_user_model, scos_memory_graph, scos_correct, scos_forget, scos_graphify_query, scos_graphify_path, scos_graphify_explain) to search, inspect, and manage persistent cross-session memory, including the graphify codebase knowledge graph. Use when the user says "recall", "what do you remember about", "check memory status", "why do you believe/remember", "forget that", "that's wrong, it's actually", or asks how parts of the codebase connect.
---

This plugin installs from the Codex plugin marketplace (`codex plugin add
scos-memory-fabric@scos-fabric-tools`, see
`scripts/install_agent_plugins.py`) rather than being copied into a
user-config directory the way the Cline/Cursor/Windsurf bundles are. Its
`hooks.json` fires the same `SessionStart` reminder Claude Code gets — call
`scos_recall` before answering from assumption — but unlike Claude Code,
**there are no slash commands here**: Codex's plugin SDK doesn't expose a
command surface, so every recall, explain, or status check happens through
you calling the tool directly, never through a user-typed shortcut. If the
user wants an explicit lookup, that request comes as plain text, not a
`/command`.

The bundled bridge (`scripts/scos-mcp-bridge.py`) talks to the hosted SCOS
gateway (`cloud-control-plane` fronting `mcp-gateway`), reading your own
per-user API key from `~/.hermes/scos.env` on this machine (see
`docs/agent-connection-guide.md` §2 for how that file is populated). It
exposes ten tools, scoped to the tenant your API key belongs to.

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
| `scos_graphify_query` | Ask a plain-language question against the graphify codebase knowledge graph | `q` |
| `scos_graphify_path` | Trace the shortest path between two concepts in the graph | `from`, `to` |
| `scos_graphify_explain` | Explain one node in the graph and list its connections | `node` |

## When to use each

- **Start of nontrivial work, or when the user references past context**
  ("we talked about this before", "what did we decide about X") → call
  `scos_recall` with a focused query before answering from assumption. The
  session-start hook is only a one-time reminder — recall again whenever
  the topic shifts.
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
- **User asks how two things in the codebase connect, or wants a scoped
  answer instead of grepping** → `scos_graphify_query` for an open
  question, `scos_graphify_path` for two named things, `scos_graphify_explain`
  for one node's connections.

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
- `integrations/codex/README.md` — how this plugin installs from the
  marketplace.
- `integrations/claude/` — the equivalent Claude Code / Claude Desktop
  packaging, same backend but with slash commands this bundle doesn't have.
