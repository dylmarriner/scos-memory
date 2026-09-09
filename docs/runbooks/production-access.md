# Production access: hermes

> Operational snapshot recorded 2026-09-07. Verify the host, checkout, service
> health, and certificate state before relying on any detail below.

The only currently running deployment of scos-memory-fabric lives on the
Linode instance nicknamed **hermes**.

## Host

| | |
|---|---|
| Linode label | `ubuntu-sg-sin-2` |
| Linode ID | 101028054 |
| IPv4 | 172.236.155.15 |
| Region | sg-sin-2 (Singapore) |
| Type | g6-standard-4 (4 vCPU / 8GB) |
| OS | Ubuntu 24.04 |
| Public hostname | `scos.172.236.155.15.nip.io` (nip.io wildcard DNS, no real domain registered) |

Manage the instance itself (reboot, resize, backups) through the Linode
Cloud Manager or API against instance ID 101028054.

## SSH

```bash
ssh root@172.236.155.15
```

Key-based auth only, no host alias configured in `~/.ssh/config` as of
2026-09-07 — add one if you use this often:

```
Host hermes
    HostName 172.236.155.15
    User root
```

## What's running

Deployed via the repo's standard Compose path (not the native/systemd path
— that's installed as a documented alternative but is not what's live
here). Checkout lives at `/opt/scos-memory-fabric`, compose project files
at `/opt/scos-memory-fabric/deploy/compose/`, secrets in
`/opt/scos-memory-fabric/.env`.

```bash
cd /opt/scos-memory-fabric
docker compose -f deploy/compose/docker-compose.yml ps
```

All Go service containers defined by the Compose file plus the Python
consolidation worker run as
`compose-<service>-1` containers, each bound to `127.0.0.1:<port>` only
(no ports exposed on the public interface directly). `compose-postgres-1`
(pgvector/pgvector:pg17) and `compose-tunnel-client-1` round out the stack.

Public entry is nginx on 443/80, terminating TLS (Let's Encrypt via
certbot, cert at
`/etc/letsencrypt/live/scos.172.236.155.15.nip.io/`) and reverse-proxying
into the container stack — see
`/etc/nginx/sites-enabled/scos-cloud-control-plane`:

- `/api/`, `/admin`, `/mcp` → `cloud-control-plane` (127.0.0.1:8079)
- everything else (`/`) → `transparency-projection` (127.0.0.1:8097)

Every other service (memory-gateway :8080, mcp-gateway :8098,
credential-vault :8100, etc.) is reachable only from the host itself —
tunnel in over SSH to hit them directly, e.g.:

```bash
ssh -L 8080:127.0.0.1:8080 root@172.236.155.15
curl -k https://127.0.0.1:8080/health/live
```

## Logs / operating it

```bash
docker compose -f /opt/scos-memory-fabric/deploy/compose/docker-compose.yml logs -f <service>
docker compose -f /opt/scos-memory-fabric/deploy/compose/docker-compose.yml ps
```

Upgrade path is the repo's normal Compose flow: `git pull` in
`/opt/scos-memory-fabric`, then `docker compose -f deploy/compose/docker-compose.yml up --build -d`.

## Caveats

- `backup-manager` was observed unhealthy on 2026-09-07 — check before relying on it for a restore.
- `nip.io` hostname means TLS/DNS depend on the IP never changing; if the instance gets rebuilt with a new IP, the cert and any bookmarked URL break.
- No Linode tags on this instance, and no host alias in `~/.ssh/config` — nothing marks it as "hermes" outside this doc, the Linode MCP account, and this session's memory writes. Anyone relying only on the repo's README/GOVERNANCE.md would find no mention of a live host; those describe only the generic reference deployment.
