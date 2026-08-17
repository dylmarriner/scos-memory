---
name: scos-recall
description: Search the SCOS Memory Fabric for context relevant to a query
user-invocable: true
---

Call the `scos_recall` MCP tool. Use whatever input the user gave when
invoking this skill as `query`; if they gave nothing, ask what they want
to recall instead of guessing a query.

Present only what the tool actually returns — do not fabricate memory
entries. If it returns nothing, say so plainly and suggest 1-2 alternative
search terms.
