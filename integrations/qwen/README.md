# Qwen Code CLI integration

Merge `mcp.json` into `~/.qwen/settings.json` under `mcpServers`, then restart
Qwen Code. The bridge reads credentials from `~/.hermes/scos.env`.

`plugin/commands/scos-recall.md`, `scos-explain.md`, `scos-status.md` give
`/scos-recall`, `/scos-explain`, `/scos-status` (Qwen Code's current
Markdown-with-frontmatter command format; the older TOML format is
deprecated) — `scripts/install_agent_plugins.py` copies them to
`~/.qwen/commands/`.
