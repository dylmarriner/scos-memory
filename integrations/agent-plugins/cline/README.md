# Cline bundle

Install the MCP server with Cline's MCP UI or `cline mcp install`, using
`mcp.json` as the server definition. Add `CLINE.md` to the workspace rules.
This is Cline's native MCP/rules integration; Cline's plugin system does not
use the Hermes `plugin.yaml` format.

Copy `workflows/*.md` into your project's `.clinerules/workflows/` for
`/scos-recall`, `/scos-explain`, `/scos-status` workflows (Cline resolves
workflows per-project, not from a global directory, so this is meant to be
checked into each repo rather than installed by the fleet-wide script).
