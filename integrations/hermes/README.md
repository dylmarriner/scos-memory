# Hermes Agent integration

The `plugins/scos-memory` directory is a native Hermes Agent plugin. Install
it into the user plugin directory with:

```bash
cp -R integrations/hermes/plugins/scos-memory ~/.hermes/plugins/scos-memory
```

It reads the existing `~/.hermes/scos.env` connection settings and discovers
the governed SCOS tool catalogue after Hermes restarts, so memory, graph,
code-intelligence, and future tools are available through one tenant-scoped
plugin. Identity fields and TLS verification are required by default.

The plugin intentionally does not declare `requires_env`: Hermes does not
inherit values from `~/.hermes/scos.env` during plugin discovery, while the
plugin itself safely loads that file at call time.

For local development only, set `SCOS_TLS_INSECURE_SKIP_VERIFY=1` explicitly;
never use that setting in production. No `/scos-recall`-style slash commands here: Hermes Agent is internal to
this fleet, not a public product, and `plugin.yaml` doesn't expose a
command-registration surface the way the Claude Code / Codex / OpenCode
plugin schemas do — only tool registration. Add one only after confirming
Hermes actually gained that capability, not by copying another agent's
pattern.
