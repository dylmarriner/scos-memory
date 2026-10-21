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
- `schema_exports`: the schema inside that backup.
- `schema_and_migration_exports`: the newest base backup has a `migration_export` (taken right after it) that unpacks against its SHA-256 digests and holds a `<version>.sql` file for every version `scos_schema_migrations` listed as applied when it was taken. The export is the SQL files under `SCOS_MIGRATION_EXPORT_PATHS` (compose: `/scos-database/migrations` and `/scos-database/policies`, mounted read only), stored through the provider like any backup; `backup-manager restore-config <export-id> <dir>` writes them back.
- `encrypted_off_host_copies` / `remote_provider_verified`: the destination is on a network filesystem (nfs, cifs, ceph, sshfs, rclone, ...) according to the kernel mount table, plus a covered base backup for the former. A local path never counts.
- `postgresql_wal_archiving`: `pg_stat_archiver` shows archiving with no failure since the last success, the last archived segment is present at the `file://` `SCOS_WAL_ARCHIVE_DESTINATION`, and archiving is at most `SCOS_WAL_ARCHIVE_MAX_SEGMENT_LAG` (default 2) segments behind. An `archive_command` that exits 0 without copying is caught by the presence check.
- `encrypted_off_host_wal`: WAL archiving is covered, the provider encrypts and is off-host, and the newest archived segment has an AES-256-GCM copy there that decrypts. With the `filesystem` provider, backup-manager ships every archived segment from `SCOS_WAL_ARCHIVE_DESTINATION` (a directory on the database host) to `<SCOS_BACKUP_DESTINATION>/wal/<segment>.enc` every `SCOS_WAL_SHIP_INTERVAL` (default 30s), so the network destination never holds WAL in the clear.
- `pitr_configured`: WAL archiving is covered, the newest verified `physical_base_backup` is younger than `SCOS_BACKUP_COVERAGE_MAX_AGE` and the WAL segment it starts from is still archived, and a `pitr_recovery` drill passed within its 7-day cadence.
- `remote_git_mirrors`: the newest recorded mirror run of `SCOS_OKF_GIT_REPO` to `SCOS_OKF_GIT_MIRROR_REMOTE` pushed with `git push --mirror`, read every ref back with `git ls-remote`, is younger than `SCOS_OKF_GIT_MIRROR_MAX_AGE` (default 48h), and the remote is another host: a URL, or a path that the kernel mount table puts on a network filesystem (a path remote is created as a bare repository on the first run). In compose the OKF store is the Git repository at `SCOS_OKF_ROOT` (default `/var/lib/scos/okf/repo` on the `okf-data` volume), seeded once from the image's approved content; backup-manager mounts it read only.
- `configuration_backups`: `SCOS_CONFIG_BACKUP_PATHS` names files or directories, the newest `configuration_backup` is younger than `SCOS_BACKUP_COVERAGE_MAX_AGE`, holds every configured path, and unpacks with every file matching the SHA-256 in its manifest.
- `object_store_snapshots`: an S3 bucket is configured (`SCOS_OBJECT_STORE_ENDPOINT`, https only), its versioning status is `Enabled`, its host resolves to no address of this host (loopback, the container's interfaces, or `SCOS_HOST_ADDRESSES`, which compose sets to the Docker host's tailnet address), and the newest covered base backup is held there as a versioned object that verifies on read-back: manifest MAC, archive SHA-256 and decryption with the backup key. See "Object-store copies" below.
- `secret_manager_recovery`: the newest secret-recovery drill, younger than `SCOS_BACKUP_COVERAGE_MAX_AGE`, read the configured Bitwarden secret or vault item (`SCOS_BACKUP_KEY_SECRET_ID`), found the key the backups are encrypted with, and decrypted the newest base backup with the key it read. See "Secret-manager recovery" below.

## Backups, fingerprints and retention

`POST /v1/backups/start` (scope `backups:admin`) takes a `base_backup` by default. The archive and a data fingerprint (row count and order-independent checksum of every public table, and a schema-catalog fingerprint) come from the same exported snapshot and are stored in the manifest. The backup is read back and verified before it is reported.

With `{"type":"physical_base_backup"}` it takes a `pg_basebackup` (tar, gzip, `--wal-method=none`, SHA-256 manifest checksums) for point-in-time recovery. It is refused unless WAL archiving is covered, because without the archive such a backup cannot be recovered. It waits until the segment holding the backup's end has been archived, then stores `base.tar.gz` and `backup_manifest` together; the manifest records the WAL range (timeline, start and end LSN and segment). The backup role needs the `REPLICATION` attribute, and in compose `deploy/compose/postgres/pg_hba.conf` (passed as `hba_file`) admits replication connections over TLS.

With `{"type":"configuration_backup"}` it takes a gzip tar of every file under `SCOS_CONFIG_BACKUP_PATHS` (comma separated absolute paths as the container sees them; mount them read only), stored through the same provider, so encrypted and off-host whenever the provider is. The manifest lists each file's size and SHA-256, and the stored archive is unpacked and checked against it before the backup is reported. To get the files back, run `backup-manager restore-config <backup-id> <target-dir>` inside the container: each file is written under the target at its original path, after every digest is checked.

With the scheduler enabled, a `base_backup` is taken once `SCOS_BACKUP_INTERVAL` (default 24h; `0` turns scheduled backups off) has passed since the newest verified one, a `configuration_backup` on the same interval when paths are configured, and a `physical_base_backup` on the same interval whenever WAL archiving is covered.

Retention is applied after every backup, on the scheduler, and by `POST /v1/retention/apply`: every backup within 35 days, the newest of each ISO week within 12 weeks and of each month within 12 months, and always the newest; archived WAL older than 35 days is removed from a `file://` WAL destination. Daily, weekly and monthly are derived from when each backup was taken, separately for `base_backup` and `physical_base_backup`. Archived WAL from the start of the oldest kept physical backup onwards is never removed, whatever its age. Unverifiable archives are never deleted automatically. Each deletion is appended (fsynced) to `retention-audit.jsonl` in `SCOS_BACKUP_DIR` before it happens, naming the actor; `GET /v1/retention/audit` returns the log.

## Object-store copies

With the `filesystem` provider and `SCOS_OBJECT_STORE_ENDPOINT`, `SCOS_OBJECT_STORE_BUCKET`, `SCOS_OBJECT_STORE_ACCESS_KEY` and `SCOS_OBJECT_STORE_SECRET_KEY_FILE` set (`SCOS_OBJECT_STORE_PREFIX` defaults to `backups/`, `SCOS_OBJECT_STORE_REGION` to `us-east-1`, `SCOS_OBJECT_STORE_CA_FILE` adds a private CA), each backup is copied as soon as it is stored and retention applied (the response's `object_store` says what was copied), and the scheduler catches up anything missed: every backup that verifies at the provider goes into the bucket as `<prefix><id>.backup` (the encrypted archive, byte for byte) and `<prefix><id>.backup.manifest.json`, reading each copy back and verifying it before counting it. backup-manager never deletes from the bucket: give it credentials that can only put, get and list (on MinIO `s3:DeleteObject` also deletes versions outright, so grant no delete action at all, and no `s3:PutBucketVersioning`), and let the bucket's lifecycle expire copies, with the current-object expiry longer than the provider's longest retention (12 months). An object overwritten by anything holding those credentials stays recoverable as a noncurrent version until the lifecycle expires it: when a backup's current objects do not verify, `object-fetch` and the coverage check try every retained version pair, newest first, and use the first that verifies, and the next sync copies the provider's copy over the damaged current object. The read actions this needs are `s3:GetObject`, `s3:GetObjectVersion`, `s3:ListBucket`, `s3:ListBucketVersions` and `s3:GetBucketVersioning`; the write action is `s3:PutObject`.

`backup-manager object-sync` runs one copy pass by hand. If the provider's copy of a backup is lost or damaged, `backup-manager object-fetch <backup-id>` verifies the object-store copy and writes it back to the provider, after which every restore path works as before.

The kubuntux deployment uses MinIO on rabuntu (`/opt/scos-minio`): built from the `RELEASE.2025-10-15T17-29-55Z` source, serving TLS with the tailnet certificate (renewed weekly by `scos-minio-cert.timer`) at `https://rabuntu.tail2c15c8.ts.net:8446`, bucket `scos-backups` with versioning and a lifecycle that expires current objects after 400 days and noncurrent versions after 90, and user `scos-kubuntux` limited by the `scos-backup-writer` policy to put, get and list (a version delete and a versioning change by that user are refused).

## Secret-manager recovery

Every encrypted backup is unreadable without `SCOS_BACKUP_ENCRYPTION_KEY`, so the key is also kept in Bitwarden, and `SCOS_BACKUP_KEY_SECRET_ID` names where. The drill reads it from one of two places.

**Bitwarden Secrets Manager.** Create a secret holding the key (the same 64 hex characters) and a machine account with read access to its project. Set `SCOS_BACKUP_KEY_SECRET_ID` to the secret's id and mount the machine account's access token at `SCOS_BWS_ACCESS_TOKEN_FILE` (`SCOS_BWS_SERVER_URL` for an EU server). The image carries `bws` 2.1.0, checked against its published SHA-256. Self-hosted Secrets Manager needs a Bitwarden Enterprise licence.

**A self-hosted vault (Vaultwarden).** Vaultwarden has no Secrets Manager, so the key is kept in a vault item instead: a login whose password is the key, or a secure note whose notes are the key. The item can be personal or in an organisation the account belongs to. Use an account that holds only what SCOS needs, because the drill can read everything that account can see.

- `SCOS_BW_SERVER_URL`: the vault, for example `https://rabuntu.tail2c15c8.ts.net:8445`. It must be https, or http only to localhost.
- `SCOS_BW_CLIENT_ID`: the account's personal API key `client_id` (`user.…`). In the web vault this is under Account settings → Security → Keys → View API key.
- `SCOS_BW_CLIENT_SECRET_FILE`: a file holding that API key's `client_secret`.
- `SCOS_BW_PASSWORD_FILE`: a file holding the account's master password.
- `SCOS_BACKUP_KEY_SECRET_ID`: the item's id (the `itemId` in the web vault's URL for it).

How the drill reads a vault item:
- It signs in with the API key as one device, whose id is kept in `secret-drill-device-id` in `SCOS_BACKUP_DIR`.
- It derives the master key on this host with the account's KDF (PBKDF2 or Argon2id) and unwraps the account key. The master password is never sent anywhere.
- It decrypts only the item it was given and checks every encrypted value's MAC. It never writes to the vault.

**Running the drill.** The drill runs on the scheduler every `SCOS_SECRET_DRILL_INTERVAL` (default 24h), and on demand with `backup-manager secret-drill`. Each run:
- reads the secret, with the token, API key and password passed in the environment or in files, never on a command line;
- checks that it is the key the backups are encrypted with;
- authenticates and decrypts the newest base backup with the key it read.

The result is kept in `secret-recovery-drill.json` in `SCOS_BACKUP_DIR`.

**Recovering the key.** If the deployment's own copy of the key is lost, read the key from Bitwarden (`bws secret get <id>`, or the vault item in the web vault), put the value in `SCOS_BACKUP_ENCRYPTION_KEY`, and restore as usual.

## Restore drills

`POST /v1/restore-tests/start` with `test_type` (scope `backups:admin`; `backup_id` defaults to the newest verified backup). `SCOS_BACKUP_RESTORE_DATABASE_URL` names the server the temporary databases are created on.

- `automated_integrity` (daily): verifies the archive against its manifest and reads its table of contents, which must hold data for every fingerprinted table. Nothing is restored.
- `monthly_restore`: restores into a temporary database and compares the key tables (`SCOS_BACKUP_KEY_TABLES`) with the manifest's row counts and checksums.
- `quarterly_full_recovery`: restores into a temporary database and compares every table and the schema catalog; a missing or extra table fails it.
- `pitr_recovery` (weekly): writes a marker to `scos_ops.pitr_markers` (migration 000059), creates a named restore point, writes a second marker, switches WAL and waits for the segment to be archived. It then unpacks the newest physical base backup into a scratch cluster (the image's `postgres` binary, a private socket, no TCP), replays WAL up to that restore point -- from the encrypted off-host copies through `backup-manager wal-fetch` when WAL is shipped, otherwise from the archive directory -- and passes only if the first marker is present, the second absent, and the recovered schema catalog matches the live one. The scratch cluster and the markers are removed afterwards. The scheduler runs it only when WAL archiving is covered and a physical backup exists.

To recover to a point in time for real:

1. Prepare the data directory. Inside the backup-manager container, run `backup-manager pitr-prepare <target-time> <empty-dir> /var/lib/scos/wal-archive` (target time in RFC 3339). The last argument is the WAL archive path as the PostgreSQL that runs the recovery sees it. The command:
   - picks the newest verified physical backup taken before the target, decrypts it, and unpacks it into the directory;
   - writes `recovery.signal`, and appends a `restore_command` that copies from the archive, `recovery_target_time` and `recovery_target_action = 'promote'` to `postgresql.auto.conf`;
   - refuses unless the archive holds every segment from the backup's start onward without a gap.
2. Stop every service that writes to the database. Move the live data directory aside (keep it until the recovery is verified), and put the prepared directory in its place, owned by the postgres user (uid 999 in the pgvector image) with mode 0700.
3. Start PostgreSQL alone. Wait for `archive recovery complete` in its log and for `pg_is_in_recovery()` to return false.
4. Remove the recovery settings with `ALTER SYSTEM RESET restore_command` (and the same for `recovery_target_time`, `recovery_target_inclusive` and `recovery_target_action`), then run `SELECT pg_reload_conf()`.
5. Start the rest of the stack. Take a new `physical_base_backup`: the recovered cluster runs on a new timeline, and point-in-time coverage needs a base backup on it.

To recover from the encrypted off-host copies instead of the archive directory, set `restore_command` to `backup-manager wal-fetch %f %p` run inside the backup-manager container, which holds the key.

`GET /v1/restore-tests/cadence` reports each cadence from passing drills of its own type only. With `SCOS_BACKUP_RESTORE_SCHEDULER_ENABLED=true` the scheduler runs overdue drills, takes the backups that are due, applies retention, and runs the OKF mirror every `SCOS_OKF_GIT_MIRROR_INTERVAL` (default 1h).
