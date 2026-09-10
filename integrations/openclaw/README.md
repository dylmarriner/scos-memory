# OpenClaw integration

SCOS integrates with OpenClaw as a **first-class memory plugin**. The plugin owns
OpenClaw's `plugins.slots.memory` slot while SCOS remains the authoritative
persistent-memory backend. OpenClaw's context engine stays separate; this
integration does not replace compaction or context-engine semantics.

The older `mcp.json` bridge and `skills/` remain useful as optional manual access
surfaces, but they are no longer the primary memory integration.

## What the plugin does

`plugin/` registers:

- OpenClaw memory capability ownership through `registerMemoryCapability()`;
- `memory_search` and `memory_get`, matching Active Memory's default custom
  provider tool names;
- `scos_recall` for explicit SCOS-native recall;
- `scos_remember` for deliberate durable writes;
- `scos_explain` for exact memory inspection and provenance;
- `scos_forget` for exact-id deletion;
- `scos_status` and `scos_memory_guidance`;
- bounded automatic recall in `before_prompt_build`;
- last-completed-turn persistence in `agent_end` rather than replaying the full
  transcript on every turn;
- session lifecycle closure through the existing SCOS provider service.

Recall and inspection use the governed SCOS MCP gateway. Durable explicit
writes and turn persistence use the existing SCOS memory lifecycle service.
The plugin does not import SCOS databases into the OpenClaw process and does not
create a second canonical memory store.

## Requirements

The package is tested against the OpenClaw 2026.9.3 plugin API cohort and Node
24.16+. The package metadata pins that build target instead of using `latest`.

SCOS must already have a tenant, principal and namespace provisioned. Production
connections should use normal TLS verification. If the SCOS internal CA is not
in the machine trust store, start OpenClaw with `NODE_EXTRA_CA_CERTS` pointing at
that CA bundle rather than disabling certificate verification.

## Configuration

Prefer environment variables for the bearer token and identity:

```bash
export SCOS_GATEWAY_URL='https://127.0.0.1:8098/mcp'
export SCOS_MEMORY_PROVIDER_URL='https://127.0.0.1:8088'
export SCOS_BEARER_TOKEN='...'
export SCOS_TENANT_ID='...'
export SCOS_PRINCIPAL_ID='...'
export SCOS_NAMESPACE_ID='...'
```

`SCOS_HERMES_PROVIDER_URL` remains accepted as a compatibility fallback for the
lifecycle service because that service currently carries the historical Hermes
name. The OpenClaw plugin itself is not coupled to Hermes configuration files.

Equivalent non-secret values may be placed under the plugin config:

```json5
{
  plugins: {
    slots: {
      memory: "scos-memory",
    },
    entries: {
      "scos-memory": {
        enabled: true,
        hooks: {
          allowPromptInjection: true,
          allowConversationAccess: true,
        },
        config: {
          gatewayUrl: "https://127.0.0.1:8098/mcp",
          providerUrl: "https://127.0.0.1:8088",
          tenantId: "...",
          principalId: "...",
          namespaceId: "...",
          autoRecall: true,
          autoCapture: true,
          recallLimit: 8,
          recallMaxChars: 4000,
          requestTimeoutMs: 8000,
        },
      },
    },
  },
}
```

`allowPromptInjection` is required for an installed external plugin to inject
automatic recall into prompt construction. `allowConversationAccess` is
required for automatic `agent_end` persistence to read completed conversation
messages. If either permission is intentionally omitted, the corresponding
automatic path is unavailable; explicit tools remain independently governed by
OpenClaw's normal tool policy.

## Install from this repository

```bash
cd integrations/openclaw/plugin
npm install
npm test
npm run build
openclaw plugins install --link .
openclaw plugins inspect scos-memory --runtime --json
```

Then select the memory slot as shown above and restart/reload the OpenClaw
Gateway as required by the installed build.

## Memory behavior

Automatic recall is bounded by `recallLimit` and `recallMaxChars`. Recalled
content is inserted with an explicit untrusted-historical-data boundary so
stored text cannot silently become trusted system instructions.

Automatic capture persists only the most recent completed user/assistant pair.
The SCOS session id is either OpenClaw's UUID session id or a deterministic
UUIDv8 derived from the OpenClaw agent/session identity. Turn ids are
content-derived, making retries idempotent against the existing SCOS lifecycle
contract.

`scos_remember` uses `/v1/provider/memory_write` rather than inventing a new MCP
write method. `scos_forget` targets a concrete memory id through the governed
MCP gateway and is declared side-effecting in the plugin manifest.

## Active Memory

The plugin exposes `memory_search` and `memory_get`, so OpenClaw Active Memory's
normal default tool allowlist works without a SCOS-specific override. SCOS does
not claim ownership of OpenClaw's wiki or raw session-transcript corpora through
those compatibility tools; requests for those corpora return an explicit
unsupported/partial result instead of fabricating coverage.

## Optional legacy surfaces

- `mcp.json` exposes the SCOS MCP bridge directly when a separate MCP surface is
  wanted.
- `skills/scos-recall/`, `skills/scos-explain/` and `skills/scos-status/` are
  manual skill surfaces.

Do not treat those as separate memory authorities. They all lead back to the
same SCOS services.
