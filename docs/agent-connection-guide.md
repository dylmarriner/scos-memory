# Connecting Agents to the SCOS Memory Fabric (rabuntu)

This is the setup guide for pointing any MCP-capable coding agent at the
`hermes-provider` instance running on the Raspberry Pi 5 (`ssh rabuntu`,
Tailscale IP `100.68.0.96`). One shared memory backbone, many clients.

## 1. How it fits together

```
 agent (Claude Code / Codex / Kilo / OpenCode / OpenClaw / Qwen / hermes-agent)
   │  stdio (JSON-RPC, MCP)
   ▼
 scos-mcp-bridge.py            ~/.hermes/scripts/scos-mcp-bridge.py
   │  HTTPS + Bearer token
   ▼
 hermes-provider  (rabuntu, :8088, TLS)   ── services/hermes-provider
   │
   ▼
 memory-gateway (:8080) ── PostgreSQL + pgvector  (authoritative store)
```

Every agent talks to the **same** bridge script, which talks to the **same**
provider, so all agents share one memory. The bridge exists because most
agent CLIs speak MCP over **stdio**, while `hermes-provider` only speaks
**HTTPS**. The bridge translates one to the other and injects auth headers
so individual agent configs never need to embed the bearer token in plain
JSON alongside everything else.

Tools exposed to every connected agent: `scos_recall`, `scos_status`,
`scos_explain`, `scos_user_model`, `scos_memory_graph`, `scos_correct`,
`scos_forget`.

## 2. One-time prerequisites (already done on this machine, listed for a fresh box)

1. **Network reachability to rabuntu.** Either Tailscale (`100.68.0.96`) or
   plain LAN/SSH (`ssh rabuntu`) must reach the Pi. Verify:
   ```bash
   ssh rabuntu 'hostname'
   curl -sk https://100.68.0.96:8088/health/live
   ```
2. **Shared bridge config** at `~/.hermes/scos.env` (mode `600`, never commit
   this — it holds the bearer token):
   ```bash
   SCOS_HERMES_PROVIDER_URL=https://100.68.0.96:8088
   SCOS_HERMES_BEARER_TOKEN=<internal service token>
   SCOS_TENANT_ID=<tenant uuid>
   SCOS_PRINCIPAL_ID=<principal uuid>
   SCOS_NAMESPACE_ID=<namespace uuid>
   SCOS_TLS_CA_PATH=~/.hermes/scos-ca.crt
   ```
3. **CA cert** at `~/.hermes/scos-ca.crt`, matching the cert `hermes-provider`
   serves (self-signed / internal CA — the bridge falls back to
   `CERT_NONE` if this file is missing, so it degrades but still connects).
4. **The bridge script** itself: `~/.hermes/scripts/scos-mcp-bridge.py`.

Steps 2–4 are per-machine, not per-agent — set them up once, then every
agent below just points at the same script with a different `SCOS_AGENT_NAME`.

## 3. Per-agent config

Every entry below is identical except for `SCOS_AGENT_NAME`, which the
bridge uses to derive a stable session bucket
(`uuid5(NAMESPACE_DNS, "scos-mcp-{name}")`) — set a distinct name per agent
so recalls don't collide across tools.

### Claude Code

`~/.claude.json` → `mcpServers` (user scope, this machine only):
```json
"scos": {
  "type": "stdio",
  "command": "/usr/bin/python3",
  "args": ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
  "env": { "SCOS_AGENT_NAME": "claude-code" }
}
```
*(Already configured on this machine.)*

To share this with teammates instead of configuring it per-machine, put the
same block in a project-scoped **`.mcp.json`** at the repo root (checked
into git) — Claude Code auto-loads it for anyone who opens this project,
CLI or Desktop. Don't put the bearer token in it; it isn't needed here since
the bridge reads the token from each machine's own `~/.hermes/scos.env`.

### mcp-gateway — native remote MCP (Claude Code `http` transport, Claude Desktop)

`hermes-provider` is scoped to the Hermes MemoryProvider lifecycle protocol
only (`services/hermes-provider/cmd/server/main.go` — `/v1/provider/*`); it
is not a general-purpose MCP surface and shouldn't be extended into one.
Instead there's a dedicated **`mcp-gateway`** service
(`services/mcp-gateway`) that speaks native MCP JSON-RPC over HTTPS and
forwards each governed tool call, with the caller's own identity headers, to
whichever service actually owns it:

| tool | forwarded to |
|---|---|
| `scos_status`, `scos_recall` | `hermes-provider` `/v1/provider/tool_call` |
| `scos_explain` | `memory-intelligence` `GET /v1/facts/{id}` |
| `scos_user_model` | `memory-intelligence` `GET /v1/dashboard/user-model` |
| `scos_memory_graph` | `memory-intelligence` `GET /v1/dashboard/memory-graph` |
| `scos_correct`, `scos_forget` | not implemented anywhere in this fleet yet — the gateway reports `{"implemented": false}` rather than fabricating success |

Deployed at `https://rabuntu.tail2c15c8.ts.net:8098/mcp`
(`deploy/compose/docker-compose.yml`, container `compose-mcp-gateway-1`).
It's the **only** service in this fleet a plain external MCP client (no
custom CA trust, no `-k`) can reach directly: it serves a real
Tailscale-issued (Let's Encrypt) certificate for `rabuntu.tail2c15c8.ts.net`
at its own listener (`tls-external/`, gitignored — never commit it), while
its outbound calls to `hermes-provider`/`memory-intelligence` still verify
against the shared internal dev CA via `SCOS_TLS_CA_FILE`. The cert
auto-renews daily via cron (`deploy/scripts/renew-mcp-gateway-cert.sh`,
`17 3 * * *` on rabuntu) and only restarts the container when it actually
changes.

**Claude Code** (CLI, and Desktop's embedded Claude Code sessions, which
share `~/.claude.json`):
```bash
claude mcp add --transport http --scope user scos-remote \
  https://rabuntu.tail2c15c8.ts.net:8098/mcp \
  -H "Authorization: Bearer <SCOS_HERMES_BEARER_TOKEN>" \
  -H "X-SCOS-Tenant-ID: <tenant uuid>" \
  -H "X-SCOS-Principal-ID: <principal uuid>"
```
Verify with `claude mcp get scos-remote` — should show `✔ Connected`.
*(Already registered on this machine as `scos-remote`.)*

**Claude Desktop's account-level Connectors** (Settings → Connectors, the
same mechanism behind the `claude.ai *` remote tools — Notion, Vercel,
Zapier, etc.) can now point at `https://rabuntu.tail2c15c8.ts.net:8098/mcp`
too, since it's a real MCP-over-HTTPS endpoint with a publicly-trusted
cert. Whether that UI's "Add connector" flow accepts static bearer/custom
headers (vs. requiring OAuth) hasn't been confirmed — if it doesn't, the
Claude Code path above already covers both CLI and Desktop's embedded
sessions, which is the same tool access either way.

### Codex

`~/.codex/config.toml`:
```toml
[mcp_servers.scos]
command = "/usr/bin/python3"
args = ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"]

[mcp_servers.scos.env]
SCOS_AGENT_NAME = "codex"
```
*(Already configured on this machine.)*

### Kilo Code

`~/.config/kilo/kilo.json` → `mcp` (note: array-form `command`, key is
`environment` not `env`):
```json
"mcp": {
  "scos": {
    "type": "local",
    "command": ["/usr/bin/python3", "-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
    "environment": { "SCOS_AGENT_NAME": "kilo" }
  }
}
```

### OpenCode

`~/.opencode/mcp.json` → `mcpServers`:
```json
"scos": {
  "command": "python3",
  "args": ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
  "env": { "SCOS_AGENT_NAME": "opencode" },
  "enabled": true
}
```

### OpenClaw

`~/.openclaw/mcp.json` → `mcpServers`:
```json
"scos": {
  "command": "python3",
  "args": ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
  "env": { "SCOS_AGENT_NAME": "openclaw" },
  "enabled": true
}
```

### Qwen Code CLI

`~/.qwen/settings.json` → `mcpServers`:
```json
"scos": {
  "command": "python3",
  "args": ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
  "env": { "SCOS_AGENT_NAME": "qwen" }
}
```

### hermes-agent (brainsync)

hermes-agent's live MCP servers are declared one-per-file under
`~/.bsync_core/hermes-agent/` (see `mcp-config.json` for the existing
`brainsync` entry — single-object form, not wrapped in `mcpServers`). Add a
sibling file, e.g. `~/.bsync_core/hermes-agent/mcp-config-scos.json`:
```json
{
  "name": "scos",
  "description": "SCOS Nexus Memory Fabric",
  "command": "/usr/bin/python3",
  "args": ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
  "transport": "stdio",
  "env": { "SCOS_AGENT_NAME": "hermes-agent" }
}
```
Confirm hermes-agent actually globs this directory (or whatever settings
surface it reads) before relying on it — this repo can't see hermes-agent's
loader code.

## 4. Verifying a new connection

After wiring an agent, from that agent ask it to call `scos_status` — it
should return the provider's live status, not a transport error. To debug
the bridge directly without going through an agent:
```bash
SCOS_AGENT_NAME=debug SCOS_MCP_DEBUG=1 python3 ~/.hermes/scripts/scos-mcp-bridge.py <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"scos_status","arguments":{}}}
EOF
```
`SCOS_MCP_DEBUG=1` echoes every message to stderr so you can see the raw
JSON-RPC traffic.

## 5. Troubleshooting

| Symptom | Likely cause |
|---|---|
| Agent hangs on startup / MCP timeout | Bridge is fine (`tools/list` answers from a static list instantly) — check the agent's own MCP startup timeout instead. |
| `transport error` in bridge stderr | rabuntu unreachable — check Tailscale (`tailscale status`) or `ssh rabuntu`. |
| HTTP 401/403 from provider | `SCOS_HERMES_BEARER_TOKEN` missing/stale in `~/.hermes/scos.env`. |
| TLS handshake failure | `~/.hermes/scos-ca.crt` missing or doesn't match the cert `hermes-provider` is serving; the bridge falls back to no verification rather than failing, so a handshake failure means something is actually wrong (wrong port, no TLS, etc.), not just a missing CA. |
| `scos_recall` returns nothing / wrong bucket | Two agents sharing one `SCOS_AGENT_NAME` collide on the same session UUID — give each a unique name. |

## 6. Security notes

- `~/.hermes/scos.env` holds the bearer token in plaintext — keep it
  `chmod 600`, never commit it, never echo it into a shared doc or chat.
- All per-agent config snippets above reference the bridge script and env
  file by path; none of them embed the token directly, so agent config
  files (which are more likely to get synced/backed up/shared) stay
  secret-free.
- `hermes-provider` terminates TLS itself (`:8088`); don't add a second
  reverse proxy in front of it without also updating `scos-ca.crt` on every
  client machine.
