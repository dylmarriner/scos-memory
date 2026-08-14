# Native (systemd) deployment

A Docker-free way to run the SCOS Memory Fabric: 19 Go binaries and the
Python consolidation worker as systemd services, against a natively
installed PostgreSQL — no container runtime involved.

This is an **alternative** to [`deploy/compose/`](../compose/), not a
replacement. Compose remains fully supported; use whichever fits your host.
See the README's [Technical Highlights](../../README.md#technical-highlights)
for why you might prefer one over the other — in short: this path removes
Docker Engine's baseline daemon/networking overhead, which matters on a
small host, at the cost of managing PostgreSQL and the binaries yourself
instead of letting Compose do it.

Tested against Debian/Ubuntu (apt, systemd). The unit files are portable to
any systemd distro; `postgres-setup.sh` is Debian/Ubuntu-specific (apt
package names, `/etc/postgresql/<version>/main` layout) — adapt the package
manager calls for another distro.

## What's here

```text
deploy/native/
├── README.md                       this file
├── install.sh                      one-time: user, dirs, TLS, systemd units, env templates
├── build.sh                        builds the 19 Go binaries + consolidation-worker venv
├── postgres-setup.sh                native PostgreSQL + pgvector + TLS + tuning
├── renew-mcp-gateway-cert.sh       native equivalent of deploy/scripts/renew-mcp-gateway-cert.sh
├── systemd/
│   ├── scos-fabric.target          groups every unit; `systemctl start` brings up the whole fleet
│   ├── scos-fabric@.service        templated unit for the 19 Go services (instance = service name)
│   ├── scos-fabric-migrations.service   oneshot, runs scripts/scos-migrate before anything else
│   └── scos-consolidation-worker.service
└── env/
    ├── scos-fabric.env.example     shared base config (secrets, TLS, runtime budget)
    └── <service>.env               per-service overrides (ports, upstream URLs, store paths)
```

Every port, environment variable, and dependency ordering here is a direct
translation of [`deploy/compose/docker-compose.yml`](../compose/docker-compose.yml)
— if you're comparing the two, that file is the source of truth for *what*
each service needs; this directory is *how* to run the same thing without a
container runtime. Upstream service URLs point at `127.0.0.1:<port>` instead
of Compose service hostnames, since everything runs on one host.

## Install

```bash
git clone https://github.com/dylmarriner/scos-memory.git
cd scos-memory

# 1. System user, state directories, dev TLS cert, systemd units.
sudo deploy/native/install.sh

# 2. Native PostgreSQL + pgvector. Requires `apt-get install postgresql`
#    already done, and a password chosen for the scos role.
sudo POSTGRES_PASSWORD="$(openssl rand -base64 24)" deploy/native/postgres-setup.sh
#    ^ save that generated password -- you need it in step 3.

# 3. Edit the config install.sh wrote. At minimum: SCOS_INTERNAL_SERVICE_TOKEN,
#    SCOS_CONTROL_PLANE_TOKEN, SCOS_SECURITY_ISSUER_SECRET (each
#    `openssl rand -base64 32`), SCOS_AGENT_ENDPOINT, and SCOS_DATABASE_URL /
#    DATABASE_URL with the password from step 2.
sudo ${EDITOR:-vi} /etc/scos/scos-fabric.env

# 4. Build the binaries.
deploy/native/build.sh

# 5. Start everything.
sudo systemctl start scos-fabric.target
sudo systemctl enable scos-fabric.target   # start on boot
```

## Verify

```bash
curl -k https://127.0.0.1:8080/health/live
curl -k https://127.0.0.1:8080/health/ready
sudo systemctl status 'scos-fabric@*' scos-consolidation-worker scos-fabric-migrations
```

(`-k` skips certificate verification for the internal self-signed dev cert
`install.sh` generates — pass `--cacert /etc/scos/tls/tls.crt` instead once
you trust it, or replace it with a real CA per the comment in
`scos-fabric.env.example`.)

## Logs

Every unit logs structured JSON to stdout via `packages/httpmid.NewLogger`
(services migrated to it so far — see the README's Technical Highlights),
which journald captures natively:

```bash
journalctl -u 'scos-fabric@memory-gateway' -f
journalctl -u 'scos-fabric@*' -f                    # every service, interleaved
journalctl -u 'scos-fabric@memory-gateway' -o json | jq .
```

## Upgrading

```bash
git pull
deploy/native/build.sh
sudo systemctl restart scos-fabric.target
```

`build.sh` does an atomic swap of the binaries directory, so an in-progress
`systemctl restart` never sees a half-written binary.

## Uninstall

```bash
sudo systemctl disable --now scos-fabric.target
sudo rm /etc/systemd/system/scos-fabric*.service /etc/systemd/system/scos-fabric.target /etc/systemd/system/scos-consolidation-worker.service
sudo systemctl daemon-reload
# State and config are left in place deliberately -- remove
# /var/lib/scos, /etc/scos, /opt/scos-fabric yourself once you've
# confirmed you don't need them (that's where the durable memory data,
# secrets, and TLS keys live).
```

## Troubleshooting

- **A service won't start / restarts in a loop**: `journalctl -u scos-fabric@<name> -n 50` first; most startup failures are a missing required env var (see `docs/security-configuration.md`) and log a clear message before exiting.
- **`scos-fabric-migrations.service` fails**: check `SCOS_DATABASE_URL` in `/etc/scos/scos-fabric.env` and that `postgres-setup.sh` completed successfully (`sudo -u postgres psql -d scos_memory -c "SELECT 1"`).
- **TLS handshake errors between services**: confirm `/etc/scos/tls/tls.crt` exists and every service's `SCOS_TLS_CA_FILE` points at it (the shared base env does this by default — check a per-service override file didn't accidentally clear it).
- **`postgresql-<version>-pgvector` not found**: see the error message from `postgres-setup.sh` — add the PGDG apt repository or build pgvector from source.
