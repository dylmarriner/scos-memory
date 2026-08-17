# SCOS Memory Fabric — Windsurf / Devin

This bundle covers two different long-running agents that share one bridge:
Windsurf's Cascade flows and Devin's unsupervised background sessions. Both
can run many steps without a human watching in real time, so call
`scos_recall` early and explicitly — at the start of a session or task, not
opportunistically later — since there may be no natural pause to catch a
wrong assumption before it propagates through several autonomous steps.

`scos_status`, `scos_explain`, `scos_user_model`, and `scos_memory_graph`
are read-only. `scos_correct` and `scos_forget` are mutations: because these
sessions can run unattended, never call either without an explicit,
already-given go-ahead for that specific `memory_id` — don't infer consent
from the session's general instructions.

This bridge is scoped to this bundle's memory tier (recall/status/explain/
user_model/memory_graph/correct/forget) — code-intelligence and governance
tools are not exposed here.
