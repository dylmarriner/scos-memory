# PostgreSQL Backup And Recovery

## Providers

`SCOS_BACKUP_PROVIDER` selects one of the implemented providers:

- `disabled` (the default): no backups, and the plan reports nothing covered.
- `local`: real `pg_dump --format=custom` archives kept unencrypted in `SCOS_BACKUP_DIR` on this host. For development; never counted as encrypted or off-host coverage.
- `filesystem` (aliases `postgres`, `postgresql`): archives encrypted with AES-256-GCM (`SCOS_BACKUP_ENCRYPTION_KEY`: 32 raw bytes, 64 hex characters or base64) and HMAC-authenticated manifests, written to `SCOS_BACKUP_DESTINATION` (an absolute path or `file://` URL). The database URL must use `sslmode=require` or stronger.

Any other name (`s3`, `gcs`, `azure`, ...) has no implementation: the service reports itself unavailable and covers nothing.

## What the plan reports

`GET /v1/plan` derives every §31 item from evidence at the moment it is asked; no environment flag asserts coverage.

- `base_backups`: the provider just accepted, returned and removed a verified write, and the newest backup verifies against its manifest and is younger than `SCOS_BACKUP_COVERAGE_MAX_AGE` (default 48h).
- `schema_exports`: the schema inside that backup. `schema_and_migration_exports` stays not covered: migrations are not exported.
- `encrypted_off_host_copies` / `remote_provider_verified`: the destination is on a network filesystem (nfs, cifs, ceph, sshfs, rclone, ...) according to the kernel mount table, plus a covered base backup for the former. A local path never counts.
- `postgresql_wal_archiving`: `pg_stat_archiver` shows archiving with no failure since the last success, the last archived segment is present at the `file://` `SCOS_WAL_ARCHIVE_DESTINATION`, and archiving is at most `SCOS_WAL_ARCHIVE_MAX_SEGMENT_LAG` (default 2) segments behind. An `archive_command` that exits 0 without copying is caught by the presence check.
- `pitr_configured`: not covered. Base backups are logical; WAL cannot be replayed onto them.
- `remote_git_mirrors`: the newest recorded mirror run of `SCOS_OKF_GIT_REPO` to `SCOS_OKF_GIT_MIRROR_REMOTE` pushed with `git push --mirror`, read every ref back with `git ls-remote`, is younger than `SCOS_OKF_GIT_MIRROR_MAX_AGE` (default 48h), and the remote is another host (a path is not).
- `object_store_snapshots`, `secret_manager_recovery`, `configuration_backups`: not implemented, so not covered.

## Backups, fingerprints and retention

`POST /v1/backups/start` (scope `backups:admin`) takes a `base_backup`. The archive and a data fingerprint (row count and order-independent checksum of every public table, and a schema-catalog fingerprint) come from the same exported snapshot and are stored in the manifest. The backup is read back and verified before it is reported.

Retention is applied after every backup, on the scheduler, and by `POST /v1/retention/apply`: every backup within 35 days, the newest of each ISO week within 12 weeks and of each month within 12 months, and always the newest; archived WAL older than 35 days is removed from a `file://` WAL destination. Daily, weekly and monthly are derived from when each backup was taken. Unverifiable archives are never deleted automatically. Each deletion is appended (fsynced) to `retention-audit.jsonl` in `SCOS_BACKUP_DIR` before it happens, naming the actor; `GET /v1/retention/audit` returns the log.

## Restore drills

`POST /v1/restore-tests/start` with `test_type` (scope `backups:admin`; `backup_id` defaults to the newest verified backup). `SCOS_BACKUP_RESTORE_DATABASE_URL` names the server the temporary databases are created on.

- `automated_integrity` (daily): verifies the archive against its manifest and reads its table of contents, which must hold data for every fingerprinted table. Nothing is restored.
- `monthly_restore`: restores into a temporary database and compares the key tables (`SCOS_BACKUP_KEY_TABLES`) with the manifest's row counts and checksums.
- `quarterly_full_recovery`: restores into a temporary database and compares every table and the schema catalog; a missing or extra table fails it.

`GET /v1/restore-tests/cadence` reports each cadence from passing drills of its own type only. With `SCOS_BACKUP_RESTORE_SCHEDULER_ENABLED=true` the scheduler runs overdue drills, applies retention, and runs the OKF mirror every `SCOS_OKF_GIT_MIRROR_INTERVAL` (default 1h).
