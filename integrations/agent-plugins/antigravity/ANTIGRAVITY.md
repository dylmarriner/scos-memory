# SCOS Memory Fabric — Antigravity

Antigravity's Manager surface can run several agent tasks side by side.
Call `scos_recall` per task, not once for the whole workspace — a
preference or fact recalled for one task shouldn't be assumed to apply to a
concurrent, unrelated one running in another task pane.

`scos_status`, `scos_explain`, `scos_user_model`, and `scos_memory_graph`
are read-only. `scos_correct` and `scos_forget` mutate memory shared across
every task in the workspace — confirm the exact `memory_id` in the task
where the correction was requested before calling either, since a
correction made in one task is visible to all of them.

This bridge is scoped to Antigravity's memory tier (recall/status/explain/
user_model/memory_graph/correct/forget) — code-intelligence and governance
tools are not exposed here.
