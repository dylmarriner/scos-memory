# Kilo Code integration

Install the SCOS bridge and add the following entry to
`~/.config/kilo/kilo.json` under `mcp`:

```json
"scos": {
  "type": "local",
  "command": ["/usr/bin/python3", "-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
  "environment": { "SCOS_AGENT_NAME": "kilo" }
}
```

The bridge reads credentials from `~/.hermes/scos.env`.

`commands/scos-recall.md`, `scos-explain.md`, `scos-status.md` give
`/scos-recall`, `/scos-explain`, `/scos-status` — `scripts/install_agent_plugins.py`
copies them to `~/.kilo/commands/`.
