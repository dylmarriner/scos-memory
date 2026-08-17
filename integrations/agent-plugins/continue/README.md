# Continue bundle

Register `mcp.json` in Continue's MCP configuration and include
`CONTINUE.md` in the workspace rules. The bundle is intentionally separate
from the existing model configuration.

`prompts/scos-recall.prompt`, `scos-explain.prompt`, `scos-status.prompt`
are Continue prompt files — `scripts/install_agent_plugins.py` copies them
to `~/.continue/prompts/`, giving `/scos-recall`, `/scos-explain`,
`/scos-status` in Chat/Plan/Agent mode.
