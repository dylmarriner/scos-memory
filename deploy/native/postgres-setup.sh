#!/usr/bin/env bash
# Native PostgreSQL + pgvector setup, matching deploy/compose/docker-compose.yml's
# postgres service: same tuning values, same "TLS required" posture, same
# scos/scos_memory role and database names by default. Run as root, once, on
# a Debian/Ubuntu host with the postgresql package already installed
# (`apt-get install postgresql`).
#
# This does NOT run in a container -- it configures the system PostgreSQL
# installation directly. Review it before running on a host that already
# runs other PostgreSQL workloads; it assumes this instance is dedicated to
# scos-fabric.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root (sudo)" >&2
  exit 1
fi

POSTGRES_USER="${POSTGRES_USER:-scos}"
POSTGRES_DB="${POSTGRES_DB:-scos_memory}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in the environment before running this script}"

PG_VERSION="$(psql -V 2>/dev/null | grep -oE '[0-9]+' | head -1 || true)"
if [ -z "$PG_VERSION" ]; then
  echo "psql not found -- install PostgreSQL first (apt-get install postgresql)" >&2
  exit 1
fi
PG_CONF_DIR="/etc/postgresql/$PG_VERSION/main"
PG_DATA_DIR="$(sudo -u postgres psql -Atqc 'SHOW data_directory')"

echo "==> PostgreSQL $PG_VERSION detected (conf: $PG_CONF_DIR, data: $PG_DATA_DIR)"

echo "==> pgvector extension"
if ! dpkg -l "postgresql-$PG_VERSION-pgvector" >/dev/null 2>&1; then
  if ! apt-get install -y "postgresql-$PG_VERSION-pgvector"; then
    cat >&2 <<EOF
Could not install postgresql-$PG_VERSION-pgvector from the configured apt
sources. Options:
  - Add the PGDG apt repository (https://www.postgresql.org/download/linux/debian/)
    which packages pgvector for recent PostgreSQL versions, then re-run this
    script.
  - Build pgvector from source: https://github.com/pgvector/pgvector#installation
This script cannot proceed without it -- database/migrations/*.sql assume
the vector extension is available.
EOF
    exit 1
  fi
fi

echo "==> TLS cert for PostgreSQL (SAN=localhost,127.0.0.1)"
CERT_DIR="$PG_CONF_DIR"
if [ ! -f "$CERT_DIR/server.crt" ] || [ ! -f "$CERT_DIR/server.key" ]; then
  openssl req -x509 -nodes -newkey rsa:2048 -days 365 \
    -keyout "$CERT_DIR/server.key" -out "$CERT_DIR/server.crt" \
    -subj "/O=scos-internal-dev" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
  chown postgres:postgres "$CERT_DIR/server.key" "$CERT_DIR/server.crt"
  chmod 600 "$CERT_DIR/server.key"
  chmod 644 "$CERT_DIR/server.crt"
  echo "    generated a self-signed development certificate"
else
  echo "    existing certificate found, leaving it in place"
fi

echo "==> role and database"
# P0-0: CREATEROLE (not full SUPERUSER) lets this role create the
# restricted scos_app runtime role itself, via
# database/policies/000011_app_role.sql -- scos-migrate runs that file
# connected as this role. Compose's postgres image makes its bootstrap
# POSTGRES_USER a full superuser, which already implies CREATEROLE; this
# keeps the native path's privilege bump to the minimum actually needed
# rather than matching Compose's superuser by default.
sudo -u postgres psql -v ON_ERROR_STOP=1 <<SQL
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$POSTGRES_USER') THEN
    CREATE ROLE $POSTGRES_USER LOGIN CREATEROLE PASSWORD '$POSTGRES_PASSWORD';
  ELSE
    ALTER ROLE $POSTGRES_USER CREATEROLE PASSWORD '$POSTGRES_PASSWORD';
  END IF;
END
\$\$;
SELECT 'CREATE DATABASE $POSTGRES_DB OWNER $POSTGRES_USER'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '$POSTGRES_DB')\gexec
SQL
sudo -u postgres psql -v ON_ERROR_STOP=1 -d "$POSTGRES_DB" -c "CREATE EXTENSION IF NOT EXISTS vector;"

# Tuning matches deploy/compose/docker-compose.yml's postgres `command:` block
# exactly -- see that file's comment for the sizing rationale. ALTER SYSTEM
# writes postgresql.auto.conf, which survives package upgrades; a few of
# these (shared_buffers, max_connections, max_worker_processes) require a
# restart to take effect, which this script performs at the end.
echo "==> tuning (see deploy/compose/docker-compose.yml for rationale)"
sudo -u postgres psql -v ON_ERROR_STOP=1 <<SQL
ALTER SYSTEM SET wal_level = 'replica';
ALTER SYSTEM SET ssl = 'on';
ALTER SYSTEM SET ssl_cert_file = '$CERT_DIR/server.crt';
ALTER SYSTEM SET ssl_key_file = '$CERT_DIR/server.key';
ALTER SYSTEM SET max_connections = '${SCOS_PG_MAX_CONNECTIONS:-75}';
ALTER SYSTEM SET shared_buffers = '${SCOS_PG_SHARED_BUFFERS:-512MB}';
ALTER SYSTEM SET effective_cache_size = '${SCOS_PG_EFFECTIVE_CACHE_SIZE:-1GB}';
ALTER SYSTEM SET work_mem = '${SCOS_PG_WORK_MEM:-4MB}';
ALTER SYSTEM SET maintenance_work_mem = '${SCOS_PG_MAINTENANCE_WORK_MEM:-64MB}';
ALTER SYSTEM SET random_page_cost = 1.1;
ALTER SYSTEM SET effective_io_concurrency = 100;
ALTER SYSTEM SET max_worker_processes = 4;
ALTER SYSTEM SET max_parallel_workers = 2;
ALTER SYSTEM SET max_parallel_workers_per_gather = 0;
ALTER SYSTEM SET max_parallel_maintenance_workers = 1;
ALTER SYSTEM SET min_wal_size = '128MB';
ALTER SYSTEM SET max_wal_size = '1GB';
ALTER SYSTEM SET checkpoint_completion_target = 0.9;
ALTER SYSTEM SET wal_compression = 'on';
-- archive_mode/archive_command are left at their package defaults here:
-- unlike the Compose deployment's baked-in scos-wal-archive wrapper, WAL
-- archiving on a native install needs an operator-chosen destination and
-- command, not a guessed default. Configure it manually if you need it.
SQL

echo "==> restarting PostgreSQL to apply settings requiring a restart"
systemctl restart "postgresql@${PG_VERSION}-main.service" 2>/dev/null || systemctl restart postgresql.service

echo "==> verifying TLS and vector extension"
sudo -u postgres psql -d "$POSTGRES_DB" -c "SHOW ssl;"
sudo -u postgres psql -d "$POSTGRES_DB" -c "SELECT extname, extversion FROM pg_extension WHERE extname = 'vector';"

echo
echo "==> done. Set in /etc/scos/scos-fabric.env:"
echo "    SCOS_DATABASE_URL=postgresql://$POSTGRES_USER:<password>@127.0.0.1:5432/$POSTGRES_DB?sslmode=require"
echo "    DATABASE_URL=postgresql://$POSTGRES_USER:<password>@127.0.0.1:5432/$POSTGRES_DB?sslmode=require"
echo
echo "==> P0-0: also set, for the restricted scos_app runtime role every"
echo "    service connects as (provisioned by scos-migrate the first time"
echo "    it runs database/policies/000011_app_role.sql -- pick a fresh"
echo "    high-entropy SCOS_APP_PASSWORD, it does not need to match"
echo "    POSTGRES_PASSWORD and should not):"
echo "    SCOS_APP_PASSWORD=<a different password>"
echo "    SCOS_APP_DATABASE_URL=postgresql://scos_app:<that password>@127.0.0.1:5432/$POSTGRES_DB?sslmode=require"
echo "    DATABASE_URL on every service unit (scos-fabric@.service instances,"
echo "    not the migrations unit) must point at SCOS_APP_DATABASE_URL, not"
echo "    SCOS_DATABASE_URL -- only the migrations unit keeps the owner"
echo "    connection."
echo
echo "==> and, for the scos_worker role the background workers connect as"
echo "    (database/policies/000015_worker_role.sql -- its own fresh password):"
echo "    SCOS_WORKER_PASSWORD=<a third password>"
echo "    SCOS_WORKER_DATABASE_URL=postgresql://scos_worker:<that password>@127.0.0.1:5432/$POSTGRES_DB?sslmode=require"
echo "    and SCOS_DATABASE_URL=<SCOS_WORKER_DATABASE_URL> in"
echo "    /etc/scos/env/consolidation-worker.env."
echo
echo "==> and, for the read-only scos_backup role backup-manager dumps as"
echo "    (database/policies/000016_backup_role.sql -- its own fresh password):"
echo "    SCOS_BACKUP_PASSWORD=<a fourth password>"
echo "    and in /etc/scos/env/backup-manager.env:"
echo "    SCOS_BACKUP_DATABASE_URL=postgresql://scos_backup:<that password>@127.0.0.1:5432/$POSTGRES_DB?sslmode=require"
echo "    SCOS_BACKUP_RESTORE_DATABASE_URL=<a CREATEDB role>@127.0.0.1:5432/postgres?sslmode=require (restore drills)"
echo "    SCOS_BACKUP_RESTORE_TEMPLATE=<template database with the schema's extensions,"
echo "    owned by that role> when it is not a superuser (pgvector is untrusted)"
