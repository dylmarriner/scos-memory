---
name: scos-memory
description: Use the SCOS Memory Fabric's mcp-gateway tools (scos_recall, scos_status, scos_explain, scos_user_model, scos_memory_graph, scos_correct, scos_forget) to search, inspect, and manage the user's persistent cross-session memory. Use when the user says "recall", "what do you remember about", "check memory status", "why do you believe/remember", "forget that", "that's wrong, it's actually", or otherwise references something they told Claude in a past session.
---

Call `scos_recall` with a query relevant to the current topic before
answering from assumption about something the user may have said in a past
session — don't wait to be asked. One recall doesn't cover a whole
conversation; call it again whenever the topic shifts to something the
earlier recall wouldn't have covered.

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

- **User asks something that a past session might already answer** → `scos_recall` first, with a query specific to the current question. Present only what the tool actually returns — never fabricate a memory. Report "nothing found" only after the tool actually returns empty.
- **User asks whether memory / the connection is working** → `scos_status`. If it fails, tell them to check `gateway_url`, `bearer_token`, `tenant_id`, and `principal_id` in this plugin's config.
- **User asks "why do you think/remember that" about a specific fact** → `scos_explain`, using the `memory_id` from a prior `scos_recall` result — don't invent one.
- **User asks what you know about them, broadly** → `scos_user_model`.
- **User asks how memories relate to each other, or wants a broader picture** → `scos_memory_graph`.
- **User says a stored fact is wrong** → `scos_correct`, quoting back the corrected value before calling, since it's a mutation.
- **User explicitly asks to forget/delete something** → `scos_forget`, only after confirming the specific `memory_id` and getting the user's explicit go-ahead — this is destructive and not easily undone.

## Caution: `scos_correct` and `scos_forget`

On some SCOS deployments these two tools are wired into the tool list but
have no backing implementation yet (the server routes them to a stub).
Calling one may return an error or silently no-op. Tell the user plainly if
a call doesn't behave as expected — don't assume silence means success.

## Anti-patterns

WRONG: calling `scos_forget` because the user seems annoyed about something
being remembered, without asking which specific memory.

RIGHT: ask for (or look up via `scos_recall`/`scos_memory_graph`) the exact
`memory_id`, restate what will be forgotten, and get confirmation first.

WRONG: treating a lack of proactive recall as fine because "nothing came up
before."

RIGHT: call `scos_recall` with a query relevant to what's actually being
asked right now — a stale or generic query returns stale or generic results.
