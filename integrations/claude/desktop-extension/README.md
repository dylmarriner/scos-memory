# scos-memory-fabric (Claude Desktop extension)

Claude Desktop only launches local **stdio** MCP servers via its Desktop
Extensions (`.mcpb`) mechanism — it can't point an `.mcpb` bundle at a
remote HTTP endpoint directly. `mcp-gateway` (`services/mcp-gateway`) is
remote, so this extension bundles a small Node.js relay (`server/index.js`,
no third-party dependencies) that Claude Desktop runs locally over stdio; the
relay forwards every JSON-RPC message to `mcp-gateway`'s Streamable HTTP
endpoint with your auth headers attached, and streams the responses back.
Functionally this is the same path Claude Code's native `http` transport
uses — see `integrations/claude/plugins/scos-memory-fabric` for that side.

If your Claude Desktop version's **Settings → Connectors → Add custom
connector** UI turns out to accept a remote URL with static bearer/custom
headers (unconfirmed as of `docs/agent-connection-guide.md` — some versions
only support OAuth-based connectors), you can skip this extension entirely
and point that UI straight at the gateway URL below. This extension exists
for the case where it doesn't.

## Build the `.mcpb` bundle

```bash
npm install -g @anthropic-ai/mcpb
cd integrations/claude/desktop-extension
mcpb pack
```

This produces `scos-memory-fabric.mcpb` in this directory.

## Install

Double-click the `.mcpb` file (or drag it into Claude Desktop) to open the
install dialog. Claude Desktop prompts for four values — same ones as the
Claude Code plugin, read from your own `~/.hermes/scos.env`:

| Prompt | Source |
|---|---|
| mcp-gateway URL | defaults to the rabuntu deployment (`https://rabuntu.tail2c15c8.ts.net:8098/mcp`) |
| SCOS bearer token | `SCOS_HERMES_BEARER_TOKEN` in `~/.hermes/scos.env` |
| Tenant ID | `SCOS_TENANT_ID` in `~/.hermes/scos.env` |
| Principal ID | `SCOS_PRINCIPAL_ID` in `~/.hermes/scos.env` |

The bearer token field is `sensitive` — Claude Desktop stores it in the OS
keychain, not in a plaintext config file.

## Verifying / debugging

Ask Claude (in a chat using this connector) to call `scos_status`. To watch
the relay's own stderr logging while testing manually:

```bash
SCOS_GATEWAY_URL=https://rabuntu.tail2c15c8.ts.net:8098/mcp \
SCOS_BEARER_TOKEN=<token> SCOS_TENANT_ID=<uuid> SCOS_PRINCIPAL_ID=<uuid> \
node server/index.js <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"scos_status","arguments":{}}}
EOF
```

Responses print to stdout as one JSON-RPC line each; relay diagnostics
(transport errors, non-2xx HTTP status, SSE parse failures) print to stderr
prefixed `[scos-mcp-relay]`.

## Known limitations

- `scos_correct` and `scos_forget` are not implemented anywhere in this
  fleet yet — the gateway reports `{"implemented": false}` for those calls
  (see `docs/agent-connection-guide.md`), this relay just forwards whatever
  the gateway returns.
- The relay does not retry or reconnect a dropped session; if `mcp-gateway`
  rotates its session, Claude Desktop's own MCP client re-initializes and a
  fresh session is negotiated automatically.
- Only tested against `mcp-gateway`'s Streamable HTTP framing (plain JSON or
  single-block SSE responses). If the gateway ever starts pushing
  server-initiated multi-event SSE streams mid-request, extend
  `parseSse`/`relay` in `server/index.js` accordingly.
