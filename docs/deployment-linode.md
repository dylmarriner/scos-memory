# SCOS deployment on Linode

This is the verified deployment record as of 2026-08-30. Secrets are
intentionally excluded.

## Host

SCOS is deployed on the 8 GB Linode instance:

- Linode label: `ubuntu-sg-sin-2`
- Linode ID: `101028054`
- Public IPv4: `172.236.155.15`
- Region: `sg-sin-2`
- Deployment checkout: `/opt/scos-memory-fabric`
- Deployment mode: Docker Compose, using `deploy/compose/docker-compose.yml`

The second running Linode, `tadpods-sg-sin-2-4gb` (`172.237.69.182`), was not
identified as the SCOS host.

## Runtime topology

The Compose stack runs PostgreSQL and the SCOS services, including
`hermes-provider`, `memory-gateway`, `memory-intelligence`, `mcp-gateway`,
`cloud-control-plane`, graphify, code intelligence, and worker services.
The containers are healthy on the host except for the currently reported
`transparency-projection` and `backup-manager` healthchecks.

The internal listeners are bound to loopback:

- `cloud-control-plane`: `127.0.0.1:8079`
- `mcp-gateway`: `127.0.0.1:8098`

Nginx is the public TLS ingress, configured for:

```text
https://scos.172.236.155.15.nip.io
```

The MCP URL for clients using a `scos_live_...` API key is:

```text
https://scos.172.236.155.15.nip.io/mcp
```

## Authentication and client wiring

The `scos_live_...` key is accepted by `cloud-control-plane`, which derives
tenant/principal identity, mints a short-lived internal token, and proxies to
`mcp-gateway`. The direct `mcp-gateway:8098` endpoint expects an internal
signed bearer token and is not the correct endpoint for a cloud API key.

The local agent bridge reads its secret configuration from
`~/.hermes/scos.env`; the relevant setting is:

```text
SCOS_GATEWAY_URL=https://scos.172.236.155.15.nip.io/mcp
```

Keep the API key in that mode-600 machine-local file. Never commit it or put
it in a plugin manifest or this repository.

## Verification

Both internal health endpoints returned HTTP 200 on the host during
verification on 2026-08-30. From a client, check the public route and invoke
`scos_status` through the configured MCP client:

```bash
curl -k -sS -o /dev/null -w '%{http_code}\n' \
  https://scos.172.236.155.15.nip.io/health/live
```

If the public route fails, check Nginx, the Linode firewall, and the Compose
stack on `/opt/scos-memory-fabric` before changing credentials.
