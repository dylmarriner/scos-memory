# Windsurf / Devin Desktop bundle

Add `mcp.json` through the editor's MCP settings and add `WINDSURF.md` to the
workspace rules. The installed desktop binary identifies itself as Devin;
the stable SCOS identity is `windsurf-devin`.

Copy `workflows/*.md` into your project's `.windsurf/workflows/` for
`/scos-recall`, `/scos-explain`, `/scos-status` workflows (Cascade resolves
workflows per-project, so this is meant to be checked into each repo rather
than installed by the fleet-wide script).
