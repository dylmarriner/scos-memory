# Scaling and high availability

The reference deployment (`deploy/compose/docker-compose.yml`) is a single-host
Docker Compose stack — the Dockerfile explicitly optimizes for a small ARM
host ("On four Cortex-A76 cores that duplication dominated deploy time"), not
a multi-node cluster. This document is the honest map of what a horizontally
scaled or highly-available deployment of the same architecture would need. It
is not a claim that HA is implemented today.

## What already supports scale-out

- **Services are stateless HTTP over PostgreSQL.** Every governed service in
  `services/` reads its identity and config from environment variables and
  authoritative state from Postgres — nothing is held in an in-process cache
  that a second replica couldn't also read. Running N copies of
  `memory-gateway` behind a load balancer works today with no code change,
  *except* for the services listed below.
- **PostgreSQL is the single source of truth**, so the hard part of scaling
  this system is scaling Postgres, not the Go services in front of it —
  standard territory: streaming replication for read replicas, PgBouncer or
  equivalent for connection pooling under many replicas, and a supervised
  failover mechanism (Patroni, or a managed Postgres HA offering) for the
  primary.
- **Zero-secret-default startup** and the shared internal bearer/control-plane
  token model (`docs/security-configuration.md`) already work identically
  whether there is one replica or many — auth doesn't assume a single
  process.
- **Graceful shutdown** (`packages/tlsserve.ServeGracefully`, rolled out to
  the three flagship services — see the README's Technical Highlights) means
  a rolling deploy or autoscaler scale-down drains in-flight requests instead
  of dropping them, which is a prerequisite for safe horizontal scaling, not
  just single-host hygiene.

## What blocks naive horizontal scaling today

A handful of services keep state on local disk (see the Dockerfile's
`/var/lib/scos/*` directories), which makes them **not** safely replicable
without further work:

| Service | Local state | What replicating it needs |
|---|---|---|
| `backup-manager` | `/var/lib/scos/backups` | Shared object storage (S3-compatible) instead of a local directory, or pin to a single replica. |
| `execution-proxy` | `/var/lib/scos/execution-proxy/{raw,state}` | Same — shared storage, or single-writer semantics. |
| `code-intelligence` | `/var/lib/scos/code-intelligence` | The index itself would need to live in Postgres/object storage rather than on-disk per replica, or each replica needs its own full rebuild. |
| `observability` | `/var/lib/scos/observability` | Route metrics to a real time-series backend instead of local files once running more than one replica. |
| `okf-synchroniser` | `/okf` (baked into the image) + `wal-archive` | OKF content is already Git-authoritative and rebaked per image; WAL archiving needs a shared destination, not local disk, for more than one archiver. |
| `agent-registry` | `/var/lib/scos/agent-registry/state` | Registry state should already be in Postgres per the service's persistence migration (`database/migrations/000012_agent_registry_persistence.sql`) — verify no local-only fallback path is load-bearing before scaling this one out. |

None of this is exotic — it's the standard "find the stateful services and
either externalize their storage or pin them to one replica" pass every
system does before going multi-node. It hasn't been done here because the
deployment target hasn't needed it.

## A reference path to multi-node, if it's ever needed

1. **Externalize the state above** (object storage for backups/execution-proxy
   artifacts, a real metrics backend for observability, Postgres for anything
   still on local disk).
2. **Put a reverse proxy / load balancer in front of the stateless services**
   (nginx, Traefik, or a cloud load balancer), replacing Compose's direct
   port publishing.
3. **Move Postgres to a managed or self-hosted HA setup** (streaming
   replication + failover), since it's the one component that can't simply be
   replicated without care.
4. **Replace Compose with an orchestrator** (Kubernetes, Nomad, or a
   multi-host Compose/Swarm setup) once there's more than one host to
   schedule across. A raw Kubernetes `Deployment`/`Service` pair for a
   stateless service like `memory-gateway` is a direct translation of its
   existing Compose service definition (env vars, health/readiness probes on
   `/health/live` and `/health/ready`, no volume mounts) — the work is in
   step 1, not in writing YAML.

Steps 1–3 are worth doing regardless of orchestrator choice. Step 4 is only
worth doing once there's an actual second host to schedule onto.
