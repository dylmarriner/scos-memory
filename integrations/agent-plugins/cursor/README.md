# Cursor bundle

Add `mcp.json` through Cursor's MCP settings or with Cursor's `--add-mcp`
option, then add `CURSOR.md` to the project rules. Cursor has no portable
third-party plugin manifest for this workflow, so this is its native
MCP/rules package.

Copy `commands/*.md` into your project's `.cursor/commands/` for
`/scos-recall`, `/scos-explain`, `/scos-status` (Cursor resolves custom
commands per-project, so this is meant to be checked into each repo rather
than installed by the fleet-wide script).
