# SCOS Memory Fabric — Cursor

Cursor's Agent/Composer mode often edits several files in one pass before
you review a combined diff. Call `scos_recall` before that pass starts if
the edit depends on prior context or a stated preference — don't let it
surface mid-diff where it's hard to separate from the edit itself.

`scos_status`, `scos_explain`, `scos_user_model`, and `scos_memory_graph`
are read-only; call them freely, including mid-conversation to check a
claim. `scos_correct` and `scos_forget` mutate stored memory — quote the
exact `memory_id` and the change back to the user in chat (not buried in a
file diff) before calling either.

This bridge is scoped to Cursor's memory tier (recall/status/explain/
user_model/memory_graph/correct/forget) — code-intelligence and governance
tools are not exposed here.
