# Connecting Agents to the SCOS Memory Fabric (kubuntux)

This is the setup guide for pointing any MCP-capable coding agent at the
SCOS stack running locally on kubuntux (Tailscale IP `100.93.75.87`,
Docker Compose). One shared memory backbone, many clients.

## 1. How it fits together

```
 agent (Claude Code / Codex / Kilo / OpenCode / OpenClaw / Qwen / hermes-agent)
   │  stdio (JSON-RPC, MCP)
   ▼
 scos-mcp-bridge.py            ~/.hermes/scripts/scos-mcp-bridge.py
   │  HTTPS + Bearer token
   ├──► hermes-provider        (kubuntux, :8088, TLS)  — 7 memory tools
   │      └──► memory-gateway (:8080) ── PostgreSQL + pgvector
   └──► code-intelligence     (kubuntux, :8084, TLS, control-plane)  — 3 code tools
          └──► /v1/index, /v1/query, /v1/impact
```

Every agent talks to the **same** bridge script, which talks to the **same**
services, so all agents share one memory and one code index. The bridge
exists because most agent CLIs speak MCP over **stdio**, while the SCOS
services speak **HTTPS**. The bridge translates one to the other and injects
auth headers so individual agent configs never need to embed the bearer
token in plain JSON alongside everything else.

Tools exposed to every connected agent: `scos_recall`, `scos_status`,
`scos_explain`, `scos_user_model`, `scos_memory_graph`, `scos_correct`,
`scos_forget` (memory, via hermes-provider) plus `scos_graphify_query`,
`scos_graphify_path`, `scos_graphify_explain` (graphify knowledge-graph
traversal, via graphify-provider :8099), and `scos_code_repo_register`,
`scos_code_repo_list`, `scos_code_repo_get`, `scos_code_index`,
`scos_code_query`, `scos_code_impact` (code intelligence, via
code-intelligence :8084 — needs `SCOS_CONTROL_PLANE_TOKEN` in scos.env). A
repository must be registered with `scos_code_repo_register` (or `POST
/v1/repos` directly) before `scos_code_index` will accept files for it --
`scos_code_index` 404s against an unregistered `repository_id`.

## 2. One-time prerequisites (already done on this machine, listed for a fresh box)

1. **Network reachability to kubuntux.** Tailscale (`100.93.75.87`) must
   reach the stack. Verify:
   ```bash
   curl -sk https://100.93.75.87:8088/health/live
   curl -sk https://100.93.75.87:8084/health/live
   ```
2. **Shared bridge config** at `~/.hermes/scos.env` (mode `600`, never commit
   this — it holds the bearer token):
   ```bash
   SCOS_HERMES_PROVIDER_URL=https://100.93.75.87:8088
   SCOS_HERMES_BEARER_TOKEN=<internal service token or claims JWT>
   SCOS_CODE_INTELLIGENCE_URL=https://100.93.75.87:8084
   SCOS_CONTROL_PLANE_TOKEN=<control plane token>
   SCOS_TENANT_ID=<tenant uuid>
   SCOS_PRINCIPAL_ID=<principal uuid>
   SCOS_NAMESPACE_ID=<namespace uuid>
   SCOS_TLS_CA_PATH=~/.hermes/scos-ca.crt
   ```
3. **CA cert** at `~/.hermes/scos-ca.crt`. The internal cert is a self-signed
   leaf (O=scos-internal-dev) and cannot verify as a trust anchor, so the
   bridge matches the scos-nexus plugin and disables peer verification
   (`CERT_NONE`) — the file is optional for compatibility.
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
| `scos_memory_summary` | `memory-intelligence` `GET /v1/dashboard/summary` — real LLM summary when `DEEPSEEK_API_KEY` is configured, otherwise a deterministic network-grouped digest (response's `mode` field says which) |
| `scos_graphify_query` | `graphify-provider` `POST /v1/graphify/query` |
| `scos_graphify_path` | `graphify-provider` `POST /v1/graphify/path` |
| `scos_graphify_explain` | `graphify-provider` `POST /v1/graphify/explain` |
| `scos_correct` | `memory-intelligence` `PATCH /v1/facts/{id}` — inserts a new fact with the corrected content and marks the old one `superseded` (facts are append-only; nothing is mutated in place) |
| `scos_forget` | `memory-intelligence` `DELETE /v1/facts/{id}[?cascade=true]` — soft-deletes the fact (`status='revoked'`); idempotent. `cascade=true` also revokes the fact's superseded predecessor chain |
| `scos_correct_batch` | `memory-intelligence` `POST /v1/facts/batch-correct` — up to 100 corrections in one call, each independent |
| `scos_forget_batch` | `memory-intelligence` `POST /v1/facts/batch-forget` — up to 100 forgets by ID in one call, each independent |
| `scos_fact_feedback` | `memory-intelligence` `POST /v1/facts/{id}/feedback` — records POSITIVE/NEGATIVE/VERY_NEGATIVE quality signal against a fact (stored only, no automatic action taken) |
| `scos_fact_history` | `memory-intelligence` `GET /v1/facts/{id}/history` — full append-only revision history for a fact, including versions from before a correction or after a forget |
| `scos_forget_all` | `memory-intelligence` `DELETE /v1/facts?confirm=true[&namespace_id=...]` — bulk soft-delete: one namespace, or (omitting `namespace_id`) every active fact for the tenant. Requires `confirm=true` |
| `scos_code_repo_register` | `code-intelligence` `POST /v1/repos` — register a repository (canonical path, remote identity, branch, language set) and get back the `repository_id` to index/query/impact against |
| `scos_code_repo_list` | `code-intelligence` `GET /v1/repos` — list repositories registered for the caller's tenant |
| `scos_code_repo_get` | `code-intelligence` `GET /v1/repos/{id}` — a registered repository's details, incl. its current index generation |
| `scos_code_index` | `code-intelligence` `POST /v1/index` — index (or re-index) files for a repository |
| `scos_code_query` | `code-intelligence` `POST /v1/query` — query the code index by symbol/path/intent |
| `scos_code_impact` | `code-intelligence` `POST /v1/impact` — change-impact set for modified symbols |

Deployed locally at `https://100.93.75.87:8098/mcp` (kubuntux, container
`compose-mcp-gateway-1`) — this is the **full** governed tool surface (136
tools incl. the code indexer), unlike the bridge's 10. The gateway serves a
self-signed cert (valid for 127.0.0.1), so remote MCP clients need
`--insecure`/SSL-verify-off or the `tls-external/` Tailscale-issued cert
recipe from `deploy/scripts/renew-mcp-gateway-cert.sh` (gitignored — never
commit it). Its outbound calls to `hermes-provider`/`memory-intelligence`/
`code-intelligence` still verify against the shared internal dev CA via
`SCOS_TLS_CA_FILE`.

**Claude Code** (CLI, and Desktop's embedded Claude Code sessions, which
share `~/.claude.json`):
```bash
claude mcp add --transport http --scope user scos-remote \
  https://100.93.75.87:8098/mcp \
  -H "Authorization: Bearer <SCOS_...KEN>" \
  -H "X-SCOS-Tenant-ID: <tenant uuid>" \
  -H "X-SCOS-Principal-ID: <principal uuid>"
```
Verify with `claude mcp get scos-remote` — should show `✔ Connected`.
*(Already registered on this machine as `scos-remote`.)*

**Claude Desktop's account-level Connectors** (Settings → Connectors, the
same mechanism behind the `claude.ai *` remote tools — Notion, Vercel,
Zapier, etc.) can now point at `https://100.93.75.87:8098/mcp` too. Whether
that UI's "Add connector" flow accepts static bearer/custom headers (vs.
requiring OAuth) hasn't been confirmed — if it doesn't, the Claude Code path
above already covers both CLI and Desktop's embedded sessions, which is the
same tool access either way.

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
| `transport error` in bridge stderr | kubuntux unreachable — check Tailscale (`tailscale status`) or that the SCOS compose stack is up (`docker ps`). |
| HTTP 401/403 from provider | `SCOS_HERMES_BEARER_TOKEN` missing/stale in `~/.hermes/scos.env`. |
| HTTP 401/403 from code-intelligence on a `scos_code_*` call | `SCOS_CONTROL_PLANE_TOKEN` missing/mismatched in `~/.hermes/scos.env` — code tools need the control-plane token header in addition to the bearer. |
| TLS handshake failure | the internal cert is a self-signed leaf (`O=scos-internal-dev`) and the bridge disables peer verification (`CERT_NONE`) to match the plugin; a handshake failure means something is actually wrong (wrong port, no TLS, etc.), not just a missing CA. |
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

## 7. Self-hosting with no account, no bearer token (`SCOS_AUTH_MODE=local`)

Everything above (§2-3) is the hosted/managed path: a real tenant + bearer
token issued for this specific deployment. A self-hosted, single-user
instance doesn't need any of that — `mcp-gateway` can run with auth off
entirely, resolving every caller to one reserved, always-present tenant
seeded by `database/migrations/000030_local_mode_default_identity.sql`.

```bash
scripts/local_quickstart.sh
```

This generates fresh internal secrets (Postgres password, service tokens —
none of which a user or agent ever sees), brings up the fleet on
`docker-compose.yml` + `docker-compose.local.yml`, and skips
`cloud-control-plane` and `tunnel-client` (the hosted account/billing
service and the remote-exposure tunnel — neither applies to a local
no-account deployment). It prints the local gateway address when done:
`https://127.0.0.1:8098/mcp`.

Connect an agent with no token at all:

```bash
claude mcp add --transport http scos-local https://127.0.0.1:8098/mcp
```

or, for an agent that uses the shared stdio bridge (§3), set only
`SCOS_GATEWAY_URL=https://127.0.0.1:8098/mcp` in `~/.hermes/scos.env` and
leave `SCOS_BEARER_TOKEN`/`SCOS_TENANT_ID`/`SCOS_PRINCIPAL_ID` unset — the
bridge sends no auth headers when all three are absent, which is exactly
what a `SCOS_AUTH_MODE=local` gateway expects.

Do **not** point a `SCOS_AUTH_MODE=local` gateway's port at anything beyond
`127.0.0.1` (or your own trusted LAN/tailnet) — there is no bearer token
gating it, so anyone who can reach the port can read and write this
instance's memory.
