# PostgreSQL HBA bind-mount permissions: deployment preflight and recovery

SCOS Compose mounts `deploy/compose/postgres/pg_hba.conf` into PostgreSQL at
`/etc/scos/postgres/pg_hba.conf` **read-only**. In the pinned
`pgvector/pgvector:pg17` runtime, the `postgres` user has numeric UID **999**.
On Kubuntux, 2026-10-31, the host bind source was UID 1000 and mode 0600,
making the file unreadable to UID 999. PostgreSQL failed to load HBA and
restarted repeatedly. Downstream services and DNS readiness cascaded into
unhealthy status. This does **not** on its own establish database corruption.

## Deployment gate

`deploy/scripts/auto-deploy.sh` invokes
`deploy/scripts/verify_postgres_hba_access.sh` before checking whether there
is a new commit and before any checkout/build/migration/container changes.
A second check runs immediately after the fast-forward checkout, since a
replacement HBA file may have lost its ACL. Both checks are before builds,
migrations and container recreation. The rollout writes a private
`$(git rev-parse --git-path scos-pending-deploy)` marker **before** moving HEAD
and clears it only after build, migrations, recreation and image-parity checks
(and model health when enabled) succeed. If the second gate fails after HEAD
advances, repairing permissions permits the same commit to retry instead of
silently treating the checkout as deployed. Inspect the marker when diagnosing
a skipped or repeatedly failed rollout; do not delete it to silence failures.
The gate executes an isolated, network-disabled, nonprivileged, read-only
container using the locally cached `pgvector/pgvector:pg17` image from
Compose. A conflicting `SCOS_POSTGRES_IMAGE` override is rejected. It mounts **only** the
HBA source file, never database data or TLS keys, and checks readability.
Failure exits nonzero without automatically changing ownership, permissions
or running migrations. A missing local image also fails closed; the image
must be provisioned as part of the controlled bootstrap process. A Docker
startup failure is reported separately from an HBA readability failure.

Manual check on the deployment host (from the repo root):

```bash
bash deploy/scripts/verify_postgres_hba_access.sh \
  "$PWD/deploy/compose/postgres/pg_hba.conf"
```

## Targeted repair after diagnosis

Verify the image's Postgres UID, actual Compose bind source, host file mode,
ACL, and PostgreSQL logs before changing anything. On the confirmed
`pgvector/pgvector:pg17` deployment, the narrowly scoped fix was:

```bash
getfacl -cn deploy/compose/postgres/pg_hba.conf
setfacl -m u:999:r-- deploy/compose/postgres/pg_hba.conf
bash deploy/scripts/verify_postgres_hba_access.sh \
  "$PWD/deploy/compose/postgres/pg_hba.conf"
```

The ACL grants read only to numeric UID 999; `getfacl` on a host where UID
999 is assigned to another username may display that **host-side** name.
The file mode's displayed mask may change from 0600 to 0640. Do not change
`PGDATA` permissions, edit authentication rules, or expose the HBA file to
everyone as a workaround. If undoing a specifically applied ACL entry is
appropriate, use `setfacl -x u:999 <file>` only after confirming it was
not required by another process or deployment.

Verify PostgreSQL is healthy and accepts connections, followed by every
dependent SCOS service's health checks. A read-access pass is **not**
evidence that authentication, RLS, migrations, backup recovery, and prefetch
have all passed. If `pg_hba.conf` is replaced during an upgrade, POSIX ACLs
on the old inode may not carry forward. Keep this preflight enabled.

No automated permission repair is built into the deployment script. Human
review remains required for any live access-control change.
