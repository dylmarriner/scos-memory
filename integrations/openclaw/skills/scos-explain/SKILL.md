---
name: scos-explain
description: Explain why a specific SCOS memory is held and what evidence supports it
user-invocable: true
---

Call the `scos_explain` MCP tool. Use whatever input the user gave when
invoking this skill as `memory_id`; if they gave nothing, don't invent an
id — ask for it, or run `/skill scos-recall` first to find candidates and
let the user pick one.
