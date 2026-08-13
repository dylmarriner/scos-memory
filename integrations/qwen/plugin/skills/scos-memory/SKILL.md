---
name: scos-memory
description: Use SCOS Memory Fabric for prior context and governed memory operations.
---

Call `scos_recall` before relying on prior sessions or preferences. Treat
`scos_status`, `scos_explain`, `scos_user_model`, and `scos_memory_graph` as
read-only. Always confirm the exact `memory_id` before `scos_correct` or
`scos_forget`.
