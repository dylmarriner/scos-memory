# SCOS Memory Fabric — Gemini CLI

Gemini CLI runs as one continuous terminal session — there's no separate
chat pane to scroll back through, so a wrong assumption early in the
session compounds silently across every command after it. Call
`scos_recall` before the first response that depends on prior context or a
stated preference, not just when the user explicitly asks "what did we
decide."

`scos_status`, `scos_explain`, `scos_user_model`, and `scos_memory_graph`
are read-only. `scos_correct` and `scos_forget` mutate memory — echo the
exact `memory_id` and the change back to the terminal before calling
either, since there's no diff view or approval UI to catch it after the
fact.

This extension is scoped to Gemini CLI's memory tier (recall/status/
explain/user_model/memory_graph/correct/forget) — code-intelligence and
governance tools are not exposed here.
