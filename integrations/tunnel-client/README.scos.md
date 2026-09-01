# tunnel-client (OpenAI Secure MCP Tunnel) — SCOS fabric integration

Vendored from https://github.com/openai/tunnel-client (upstream clone at
`integrations/tunnel-client/`), wired into the SCOS Memory Fabric stack as a
secure outbound MCP tunnel so ChatGPT connectors and remote OpenAI control
plane agents can reach the SCOS gateway through the tunnel instead of
exposing it publicly.

## How it fits

```
ChatGPT connector / remote agent
        │  (OpenAI control plane)
        ▼
tunnel-client daemon  (kubuntux, systemd user unit)
        │  stdio
        ▼
scos-mcp-bridge.py  (~/.hermes/scripts/scos-mcp-bridge.py)
        │  HTTPS (scos_live_* account key)
        ▼
SCOS MCP gateway  (Linode 172.236.155.15, https://scos.172.236.155.15.nip.io/mcp)
        │  146 governed tools
        ▼
memory-intelligence / code-intelligence / agent-registry / ... (fabric services)
```

The bridge is the same one every agent on the fleet uses; the tunnel just
carries it over the OpenAI control plane. Identity comes from
`~/.hermes/scos.env` (`SCOS_BEARER_TOKEN` = `scos_live_*` account key,
tenant/principal = Dylan's Linode workspace).

## Files

- `integrations/tunnel-client/` — upstream tunnel-client repo (git clone)
- `integrations/tunnel-client/scos-memory.yaml` — profile template (secrets via
  `env:` refs; never commit real values)
- `deploy/native/systemd/tunnel-client-scos.service` — systemd user unit

## Install (per machine)

1. Install the `tunnel-client` binary (see upstream README; on kubuntux it is
   `~/.local/bin/tunnel-client`).
2. Create `~/.config/tunnel-client/scos-memory.yaml` from the template with the
   real `control_plane.api_key` and `control_plane.tunnel_id` (chmod 600).
3. Wire the unit:
   ```bash
   cp deploy/native/systemd/tunnel-client-scos.service ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now tunnel-client-scos.service
   ```
4. Verify:
   ```bash
   tunnel-client doctor --profile scos-memory
   systemctl --user status tunnel-client-scos.service
   cat ~/.local/state/tunnel-client/health/scos-memory.url   # /healthz /readyz /ui
   ```

## Related

- `~/.config/tunnel-client/hermes-tools.yaml` — the older multi-channel tunnel
  (hermes/lin/rabuntu/scos/linode) via `tunnel-client.service`
- `deploy/native/systemd/scos-linode-tunnel.service` — plain SSH local-forward
  to the Linode loopback ports (8088/8098/8089/8084), used by the bridge's
  provider path when the tunnel is down
