# Gemini CLI bundle

Install with `gemini extensions link integrations/agent-plugins/gemini`.
The extension includes a small launcher; it delegates to the machine's shared
`~/.hermes/scripts/scos-mcp-bridge.py` at runtime.

`commands/scos-recall.toml`, `scos-explain.toml`, `scos-status.toml` are
bundled in the extension directory and auto-discovered as `/scos-recall`,
`/scos-explain`, `/scos-status` (Gemini CLI extensions load a `commands/`
subdirectory of TOML files the same way user/project commands work).
