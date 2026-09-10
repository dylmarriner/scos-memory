# @scos/openclaw-memory-plugin

First-class OpenClaw memory provider for the SCOS Memory Fabric.

This package owns OpenClaw's memory plugin slot while leaving SCOS as the
canonical memory store. It provides automatic bounded recall, completed-turn
persistence, Active Memory-compatible `memory_search` / `memory_get` tools, and
explicit SCOS recall/write/inspect/delete tools.

## Build and test

```bash
npm install
npm test
npm run build
npm run pack:check
```

## Runtime configuration

Required values may come from `plugins.entries.scos-memory.config` or the SCOS
environment variables:

- `SCOS_GATEWAY_URL`
- `SCOS_MEMORY_PROVIDER_URL` (or legacy `SCOS_HERMES_PROVIDER_URL`)
- `SCOS_BEARER_TOKEN`
- `SCOS_TENANT_ID`
- `SCOS_PRINCIPAL_ID`
- `SCOS_NAMESPACE_ID`

Select the plugin with `plugins.slots.memory = "scos-memory"`.

For automatic recall and turn persistence, an installed external plugin also
needs the OpenClaw hook permissions `allowPromptInjection` and
`allowConversationAccess` respectively.

See `../README.md` in the SCOS repository for the complete configuration and
security notes.
