# Crash Recovery Runbook

## Purpose

Restore SCOS Memory Fabric after a process crash, host reboot or power loss without duplicating side effects or losing committed turns.

## Recovery sequence

1. Confirm PostgreSQL is healthy and migrations match the expected schema version.
2. Confirm the object store and OKF repository are accessible.
3. Start the Memory Gateway.
4. Query turns in `running` or `interrupted` state.
5. Load each session's latest checkpoint.
6. Replay events after `last_event_sequence`.
7. Identify operations in `executing` or `unknown` state.
8. Reconcile externally side-effecting operations before retry.
9. Reset expired outbox leases.
10. Resume idempotent pending work.
11. Rebuild disposable caches and indexes only after authoritative services are stable.
12. Record a recovery audit entry.

## Volumes on a drive that mounts after Docker

If the compose volumes are bind mounts on a separate drive (as on kubuntux, where they live under `/run/media/kubuntux/backup-dev-ext4/scos/data`), Docker can start at boot before that drive is mounted.

**Symptom:** every container that uses one of those volumes exits with `error while mounting volume ... no such file or directory`. The restart policy does not retry a mount failure, so the stack stays down while the services without such volumes keep running.

Use either fix:

- **Order Docker after the drive (needs root).** Add a drop-in at `/etc/systemd/system/docker.service.d/20-scos-volumes.conf`:

  ```ini
  [Unit]
  RequiresMountsFor=/run/media/kubuntux/backup-dev-ext4
  ```

  Then run `systemctl daemon-reload`.
- **Start the stack once the drive is mounted (no root).** Install a lingering user service that waits for the mount point and for Docker, then runs the deployment's `docker compose ... up -d`. On kubuntux this is `~/.config/systemd/user/scos-stack.service`, which runs `~/.local/bin/scos-stack-up`.

To recover a host already in this state, run the same `docker compose ... up -d`. It recreates the exited containers once the drive is mounted. Then follow the recovery sequence above.

## Prohibitions

- Do not delete interrupted turns to make dashboards look clean.
- Do not retry unknown non-idempotent operations without provider reconciliation.
- Do not mark readiness until mandatory dependency checks pass.
- Do not rebuild from embeddings when authoritative content exists.

## Verification

- Every accepted user message remains present.
- Every completed reply remains present.
- Interrupted chunks are visible and ordered.
- No operation has been applied twice.
- Outbox backlog declines normally.
- Retrieval can cite authoritative evidence.
