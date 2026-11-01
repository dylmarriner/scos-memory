#!/usr/bin/env bash
# Verify that the exact Pgvector/PostgreSQL runtime UID can read the
# read-only bind-mounted pg_hba.conf before starting/recreating services.
# The isolated probe neither mounts PGDATA nor accesses a network.
set -euo pipefail

if [ "$#" -ne 1 ] || [ ! -f "$1" ]; then
  echo "postgres HBA preflight: pg_hba.conf source file is missing" >&2
  exit 1
fi

hba_source="$(realpath -- "$1")"
# Keep this image identical to the postgres service in docker-compose.yml.
# An override could validate a different UID/image than the deployed service.
postgres_image="pgvector/pgvector:pg17"
if [ -n "${SCOS_POSTGRES_IMAGE:-}" ] && [ "$SCOS_POSTGRES_IMAGE" != "$postgres_image" ]; then
  echo "postgres HBA preflight: image override differs from the Compose PostgreSQL image" >&2
  exit 1
fi

# Do not silently pull a new image or weaken file permissions in production.
if ! docker image inspect "$postgres_image" >/dev/null 2>&1; then
  echo "postgres HBA preflight: local PostgreSQL image unavailable; verify/pull the deployed image first" >&2
  exit 1
fi

probe_status=0
docker run --rm --pull=never --network none --read-only \
  --user 999:999 --entrypoint /bin/sh \
  --mount "type=bind,src=$hba_source,dst=/etc/scos/postgres/pg_hba.conf,readonly" \
  "$postgres_image" -c 'test -r /etc/scos/postgres/pg_hba.conf' >/dev/null || probe_status=$?
if [ "$probe_status" -eq 1 ]; then
  echo "postgres HBA preflight: PostgreSQL UID 999 cannot read pg_hba.conf; aborting deployment" >&2
  echo "Check host permissions/ACL for the bind-mounted file; do not expose it to everyone." >&2
  exit 1
elif [ "$probe_status" -ne 0 ]; then
  echo "postgres HBA preflight: Docker probe failed (exit $probe_status); deployment aborted" >&2
  exit 1
fi

echo "postgres HBA preflight: PostgreSQL UID 999 can read pg_hba.conf"
