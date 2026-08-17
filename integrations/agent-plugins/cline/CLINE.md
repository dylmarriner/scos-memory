# SCOS Memory Fabric — Cline

Cline runs as a step-by-step agent in Plan/Act mode: every file edit or
command is proposed and needs your approval before it executes. Treat
`scos_recall` the same way — call it as a discrete, visible step before you
propose a plan that depends on prior sessions or stated preferences, so the
user sees the recall happen rather than an assumption baked into the plan.

`scos_status`, `scos_explain`, `scos_user_model`, and `scos_memory_graph` are
read-only and safe to call without asking first. `scos_correct` and
`scos_forget` are mutations: surface the exact `memory_id` and what it will
change as part of the plan step, and only act once it's approved like any
other action.

This bridge is scoped to Cline's memory tier (recall/status/explain/
user_model/memory_graph/correct/forget) — code-intelligence and governance
tools are not exposed here.
