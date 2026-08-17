---
description: Search the SCOS Memory Fabric for context relevant to a query
---

Call the `scos_recall` MCP tool with `query` set to: $ARGUMENTS

If `$ARGUMENTS` is empty, ask the user what they want to recall instead of
guessing a query.

Present only what the tool actually returns — do not fabricate memory
entries. If it returns nothing, say so plainly and suggest 1-2 alternative
search terms.
