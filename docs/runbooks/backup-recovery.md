# PostgreSQL Backup And Recovery

The backup manager has two deliberately different modes:

- The default `disabled` mode does not claim backup coverage. `local` creates synthetic test fixtures only; they are not production backups and restore drills fail instead of reporting success.
- Production mode uses `pg_dump --format=custom`, AES-256-GCM encryption, and a separate manifest containing plaintext and ciphertext hashes plus a keyed manifest MAC.

Production configuration requires:

- An explicit `SCOS_BACKUP_PROVIDER` implementing the provider boundary (`filesystem`, `s3`, `gcs`, or `azure`) and `SCOS_BACKUP_PROVIDER_VERIFIED=true` only after the destination has been checked by the operator.
- `SCOS_BACKUP_OFF_HOST_VERIFIED=true`; a local filesystem path is not an off-host backup merely because it has a provider name.
- `SCOS_BACKUP_DESTINATION` pointing at the mounted off-host/object-store path.
- `SCOS_WAL_ARCHIVE_DESTINATION` and `SCOS_PITR_ENABLED=true` for WAL/PITR coverage.
- `SCOS_BACKUP_DATABASE_URL` or `SCOS_DATABASE_URL`.
- `SCOS_BACKUP_ENCRYPTION_KEY` as 32 raw bytes, 64 hex characters, or base64.
- `SCOS_SECRET_MANAGER` and `SCOS_BACKUP_ENCRYPTION_KEY_REF`; the key value must be injected at runtime, never committed.
- `SCOS_BACKUP_RESTORE_DATABASE_URL` for restore drills. The manager creates and drops a uniquely named temporary database using `createdb` and restores it with `pg_restore`.

Missing PostgreSQL tools, keys, destinations, TLS, provider verification, WAL/PITR settings, or restore-database settings fail readiness or the operation clearly. A successful restore drill means `pg_restore` completed against the temporary database, not merely that an artifact was readable. Remote backup and PITR are never reported as configured until those checks pass.
