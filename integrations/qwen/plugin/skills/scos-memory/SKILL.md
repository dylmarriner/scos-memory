---
name: scos-memory
description: Use SCOS Memory Fabric for prior context and governed memory operations.
---

Call `scos_recall` before relying on prior sessions or preferences. Treat
`scos_status`, `scos_explain`, `scos_user_model`, `scos_memory_graph`,
`scos_graphify_query`, `scos_graphify_path`, and `scos_graphify_explain`
(the graphify codebase knowledge graph) as read-only. Always confirm the
exact `memory_id` before `scos_correct` or `scos_forget`.
