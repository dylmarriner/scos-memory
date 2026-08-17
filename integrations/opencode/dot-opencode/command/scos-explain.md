---
description: Explain why a specific SCOS memory is held and what evidence supports it
---

Call the `scos_explain` MCP tool with `memory_id` set to: $ARGUMENTS

If `$ARGUMENTS` is empty, don't invent an id — ask the user for the
`memory_id`, or run `scos_recall` first to find candidates and let the user
pick one.
