Call the `scos_explain` MCP tool. If the user gave a `memory_id` right after
`/scos-explain`, use it verbatim. If they didn't, don't invent an id — ask
for the `memory_id`, or run `scos_recall` first to find candidates and let
the user pick one.
