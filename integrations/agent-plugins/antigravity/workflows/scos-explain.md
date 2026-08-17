Call the `scos_explain` MCP tool. If the user gave a `memory_id` as part of
invoking this workflow, use it verbatim. If they didn't, don't invent an
id — ask for the `memory_id`, or run `/scos-recall` first to find
candidates and let the user pick one.
