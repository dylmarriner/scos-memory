#!/usr/bin/env bash
# Shared-host Raspberry Pi profile for the native deployment. Run as root on
# the Pi from a checkout (push.sh puts one at /opt/scos-fabric/src). Wraps
# install.sh + postgres-setup.sh and then tightens the stack so it runs
# alongside the Pi's other applications:
#   - secrets generated on first run (existing values are never replaced)
#   - GOMAXPROCS=1 / GOMEMLIMIT=64MiB and a small PostgreSQL footprint
#   - every service bound to 127.0.0.1 except mcp-gateway, which listens on the
#     Tailscale address only, with an auto-renewed Tailscale certificate
#   - scos.slice: low CPU/IO weight, OOM-killed before other apps
# Idempotent. Expects the Go binaries already in /opt/scos-fabric/bin.
set -euo pipefail
cd /  # sudo -u postgres cannot read /root as cwd
export DEBIAN_FRONTEND=noninteractive

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root (sudo)" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
PI_DIR="$REPO_ROOT/deploy/native/pi"
ENV_FILE=/etc/scos/scos-fabric.env
SERVICES="$(ls "$REPO_ROOT/services" | grep -vE '^(model-runtime|readiness)$' | tr '\n' ' ')"

get_var() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -1; }
set_var() {
  if grep -q "^$1=" "$ENV_FILE"; then
    sed -i "s|^$1=.*|$1=$2|" "$ENV_FILE"
  else
    echo "$1=$2" >>"$ENV_FILE"
  fi
}
# Generates a secret only when the variable is empty or still the template's
# CHANGE_ME value.
ensure_secret() {
  local cur; cur="$(get_var "$1")"
  if [ -z "$cur" ] || [[ "$cur" == *CHANGE_ME* ]]; then set_var "$1" "$2"; fi
}

echo "==> packages"
apt-get install -y --no-install-recommends python3-venv openssl ca-certificates curl git >/dev/null
# Install the distro's PostgreSQL once, then stay on that major version: the
# `postgresql` meta-package would jump to PGDG's newest major (and a second
# cluster) once the PGDG repo below is added.
command -v psql >/dev/null || apt-get install -y --no-install-recommends postgresql >/dev/null
PG_VERSION="$(pg_lsclusters -h 2>/dev/null | awk 'NR==1{print $1}')"
PG_VERSION="${PG_VERSION:-$(psql -V | grep -oE '[0-9]+' | head -1)}"
if ! apt-cache show "postgresql-$PG_VERSION-pgvector" >/dev/null 2>&1; then
  echo "    adding the PGDG apt repository for postgresql-$PG_VERSION-pgvector"
  install -d /usr/share/postgresql-common/pgdg
  curl -fsSL -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc
  . /etc/os-release
  echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt $VERSION_CODENAME-pgdg main" \
    >/etc/apt/sources.list.d/pgdg.list
  apt-get update >/dev/null
fi
apt-get install -y --no-install-recommends "postgresql-$PG_VERSION" "postgresql-$PG_VERSION-pgvector" >/dev/null

echo "==> slice and drop-ins"
install -m 0644 "$PI_DIR/scos.slice" /etc/systemd/system/scos.slice
for unit in scos-fabric@.service scos-consolidation-worker.service scos-fabric-migrations.service \
  scos-index-maintenance-worker.service scos-policy-evaluation-worker.service; do
  install -D -m 0644 "$PI_DIR/scos-unit.conf" "/etc/systemd/system/$unit.d/pi.conf"
done
install -D -m 0644 "$PI_DIR/postgresql.conf" /etc/systemd/system/postgresql@.service.d/scos-pi.conf

"$REPO_ROOT/deploy/native/install.sh"

echo "==> internal TLS cert + /etc/hosts for compose service names"
# Several services default their upstream URLs to Compose hostnames
# (https://credential-vault:8100, ...). Resolving those names to loopback and
# putting them in the internal cert's SAN makes those defaults work natively.
if ! openssl x509 -in /etc/scos/tls/tls.crt -noout -ext subjectAltName 2>/dev/null | grep -q "DNS:memory-gateway"; then
  san="DNS:localhost,IP:127.0.0.1"
  for s in $SERVICES; do san="$san,DNS:$s"; done
  openssl req -x509 -nodes -newkey rsa:2048 -days 825 \
    -keyout /etc/scos/tls/tls.key -out /etc/scos/tls/tls.crt \
    -subj "/O=scos-internal-dev" -addext "subjectAltName=$san" 2>/dev/null
  chown scos:scos /etc/scos/tls/tls.key
  chmod 600 /etc/scos/tls/tls.key
  chmod 644 /etc/scos/tls/tls.crt
fi
sed -i '/# BEGIN scos-fabric/,/# END scos-fabric/d' /etc/hosts
printf '# BEGIN scos-fabric\n127.0.0.1 %s\n# END scos-fabric\n' "$SERVICES" >>/etc/hosts

echo "==> secrets (first run only)"
for v in SCOS_INTERNAL_SERVICE_TOKEN SCOS_CONTROL_PLANE_TOKEN SCOS_SECURITY_ISSUER_SECRET SCOS_CREDENTIAL_VAULT_MASTER_KEY; do
  ensure_secret "$v" "$(openssl rand -base64 32)"
done
# Hex passwords: safe inside a URL without escaping.
ensure_secret SCOS_DATABASE_URL "postgresql://scos:$(openssl rand -hex 24)@127.0.0.1:5432/scos_memory?sslmode=require"
ensure_secret SCOS_APP_PASSWORD "$(openssl rand -hex 24)"
set_var SCOS_APP_DATABASE_URL "postgresql://scos_app:$(get_var SCOS_APP_PASSWORD)@127.0.0.1:5432/scos_memory?sslmode=require"
set_var DATABASE_URL "$(get_var SCOS_APP_DATABASE_URL)"
ensure_secret SCOS_WORKER_PASSWORD "$(openssl rand -hex 24)"
set_var SCOS_WORKER_DATABASE_URL "postgresql://scos_worker:$(get_var SCOS_WORKER_PASSWORD)@127.0.0.1:5432/scos_memory?sslmode=require"
# The worker reads SCOS_DATABASE_URL, which scos-fabric.env sets to the owner
# connection for migrations; its own env file (loaded after) points it at the
# scos_worker role instead.
install -o root -g scos -m 0640 /dev/null /etc/scos/env/consolidation-worker.env.new
grep -v '^SCOS_DATABASE_URL=' /etc/scos/env/consolidation-worker.env 2>/dev/null >/etc/scos/env/consolidation-worker.env.new || true
echo "SCOS_DATABASE_URL=$(get_var SCOS_WORKER_DATABASE_URL)" >>/etc/scos/env/consolidation-worker.env.new
mv /etc/scos/env/consolidation-worker.env.new /etc/scos/env/consolidation-worker.env
# backup-manager dumps as the read-only scos_backup role (the owner is filtered
# by FORCE ROW LEVEL SECURITY and cannot pg_dump); scos-migrate provisions it.
ensure_secret SCOS_BACKUP_PASSWORD "$(openssl rand -hex 24)"

# service_env FILE KEY VALUE: set KEY in /etc/scos/env/FILE.env, replacing any
# existing value (install.sh never overwrites an existing env file).
service_env() {
  local f="/etc/scos/env/$1.env"
  [ -f "$f" ] || install -o root -g scos -m 0640 /dev/null "$f"
  sed -i "/^$2=/d" "$f"
  printf '%s=%s\n' "$2" "$3" >>"$f"
}
# OKF root is a git repo the service bootstraps from the installed seed; the
# projections state file must stay outside it or every merge sees a dirty tree.
service_env okf-synchroniser SCOS_OKF_ROOT /var/lib/scos/okf/knowledge
service_env okf-synchroniser SCOS_OKF_SEED_DIR /opt/scos-fabric/okf-seed
service_env okf-synchroniser SCOS_OKF_PROTECTED_BRANCH main
service_env okf-synchroniser SCOS_OKF_SYNCHRONISER_STATE_PATH /var/lib/scos/okf/projections.json
# The periodic workers read SCOS_DATABASE_URL first, which scos-fabric.env
# sets to the owner connection (RLS-filtered to nothing); point them at scos_worker.
service_env autonomous-control-plane SCOS_CONTROL_PLANE_WORKER_DATABASE_URL "$(get_var SCOS_WORKER_DATABASE_URL)"
service_env index-maintenance-worker SCOS_DATABASE_URL "$(get_var SCOS_WORKER_DATABASE_URL)"
service_env policy-evaluation-worker SCOS_DATABASE_URL "$(get_var SCOS_WORKER_DATABASE_URL)"
service_env backup-manager SCOS_BACKUP_PROVIDER local
service_env backup-manager SCOS_BACKUP_SCHEDULE_INTERVAL "${SCOS_BACKUP_SCHEDULE_INTERVAL:-24h}"
service_env backup-manager SCOS_BACKUP_RESTORE_SCHEDULER_ENABLED true
service_env backup-manager SCOS_BACKUP_DATABASE_URL "postgresql://scos_backup:$(get_var SCOS_BACKUP_PASSWORD)@127.0.0.1:5432/scos_memory?sslmode=require"
# Restore drills restore into a temporary database created by the owner role
# (granted CREATEDB below), connected via the maintenance database.
service_env backup-manager SCOS_BACKUP_RESTORE_TEMPLATE scos_restore_template
service_env backup-manager SCOS_BACKUP_RESTORE_DATABASE_URL "$(get_var SCOS_DATABASE_URL | sed -E 's|/scos_memory\?|/postgres?|')"

echo "==> Pi runtime budget"
set_var GOMAXPROCS "${SCOS_GOMAXPROCS:-1}"
set_var GOMEMLIMIT "${SCOS_GOMEMLIMIT:-64MiB}"
set_var GOGC 50

echo "==> PostgreSQL (small-host tuning)"
owner_pw="$(get_var SCOS_DATABASE_URL | sed -E 's|^postgresql://[^:]+:([^@]+)@.*|\1|')"
systemctl daemon-reload
# max_connections covers the sum of every service's pool cap (~90) plus
# migrations/admin; unused slots cost almost nothing.
POSTGRES_PASSWORD="$owner_pw" \
  SCOS_PG_MAX_CONNECTIONS="${SCOS_PG_MAX_CONNECTIONS:-100}" \
  SCOS_PG_SHARED_BUFFERS="${SCOS_PG_SHARED_BUFFERS:-128MB}" \
  SCOS_PG_EFFECTIVE_CACHE_SIZE="${SCOS_PG_EFFECTIVE_CACHE_SIZE:-512MB}" \
  SCOS_PG_WORK_MEM="${SCOS_PG_WORK_MEM:-2MB}" \
  SCOS_PG_MAINTENANCE_WORK_MEM="${SCOS_PG_MAINTENANCE_WORK_MEM:-32MB}" \
  "$REPO_ROOT/deploy/native/postgres-setup.sh" >/dev/null
sudo -u postgres psql -qc "ALTER SYSTEM SET max_wal_size = '256MB'" -c "ALTER SYSTEM SET max_worker_processes = 2" -c "ALTER SYSTEM SET max_parallel_workers = 1" >/dev/null
systemctl restart "postgresql@${PG_VERSION}-main.service"
sudo -u postgres psql -qc "ALTER ROLE scos CREATEDB" >/dev/null
# Restore drills run as scos, which cannot create untrusted extensions
# (pgvector); they create their temporary database from this template, which
# carries every extension the schema uses.
sudo -u postgres psql -Atc "SELECT 1 FROM pg_database WHERE datname = 'scos_restore_template'" | grep -q 1 ||
  sudo -u postgres createdb -O scos scos_restore_template
for ext in vector pgcrypto $(sudo -u postgres psql -d scos_memory -Atc "SELECT extname FROM pg_extension WHERE extname <> 'plpgsql'"); do
  sudo -u postgres psql -d scos_restore_template -qc "CREATE EXTENSION IF NOT EXISTS \"$ext\"" >/dev/null
done

echo "==> bind internal services to loopback (mcp-gateway stays reachable)"
[ -f /etc/scos/env/credential-vault.env ] || install -o root -g scos -m 0640 /dev/null /etc/scos/env/credential-vault.env
grep -q '^SCOS_CREDENTIAL_VAULT_HTTP_ADDR=' /etc/scos/env/credential-vault.env ||
  echo 'SCOS_CREDENTIAL_VAULT_HTTP_ADDR=127.0.0.1:8100' >>/etc/scos/env/credential-vault.env
for f in /etc/scos/env/*.env; do
  [ "$(basename "$f")" = mcp-gateway.env ] && continue
  sed -i -E 's/^(SCOS_[A-Z_]*HTTP_ADDR)=:/\1=127.0.0.1:/' "$f"
done

echo "==> Tailscale: mcp-gateway on the tailnet only, with a Tailscale cert"
# External MCP clients reach the Pi at https://<magicdns-name>:8098; every other
# service stays on loopback. Requires `tailscale up` with HTTPS certificates
# enabled for the tailnet.
TS_NAME="$(tailscale status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')"
TS_IP="$(tailscale ip -4 | head -1)"
set_var SCOS_MCP_GATEWAY_TLS_DOMAIN "$TS_NAME"
sed -i -E "s/^SCOS_MCP_GATEWAY_HTTP_ADDR=.*/SCOS_MCP_GATEWAY_HTTP_ADDR=$TS_IP:8098/" /etc/scos/env/mcp-gateway.env
install -D -m 0644 "$PI_DIR/mcp-gateway-tailscale.conf" /etc/systemd/system/scos-fabric@mcp-gateway.service.d/tailscale.conf
install -m 0644 "$PI_DIR/scos-mcp-cert-renew.service" "$PI_DIR/scos-mcp-cert-renew.timer" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now scos-mcp-cert-renew.timer >/dev/null 2>&1
SCOS_MCP_GATEWAY_TLS_DOMAIN="$TS_NAME" "$REPO_ROOT/deploy/native/renew-mcp-gateway-cert.sh" >/dev/null 2>&1 || true
[ -f /etc/scos/tls-external/tls.crt ] || { echo "tailscale cert failed -- enable HTTPS certificates in the tailnet admin console" >&2; exit 1; }
chown root:scos /etc/scos/tls-external/tls.key
chmod 640 /etc/scos/tls-external/tls.key
echo "    https://$TS_NAME:8098 ($TS_IP)"

echo "==> graphify-provider"
if command -v graphify >/dev/null; then
  systemctl unmask scos-fabric@graphify-provider.service >/dev/null 2>&1 || true
else
  # Readiness requires the graphify CLI; without it the service is dead weight.
  systemctl mask scos-fabric@graphify-provider.service >/dev/null
  echo "    graphify CLI not installed -- masked (pipx install graphifyy, then re-run to enable)"
fi

echo "==> consolidation-worker venv"
VENV=/opt/scos-fabric/venv/consolidation
[ -x "$VENV/bin/python" ] || python3 -m venv "$VENV"
nice -n 19 "$VENV/bin/pip" install --quiet --upgrade pip
nice -n 19 "$VENV/bin/pip" install --quiet "$REPO_ROOT/workers/consolidation"

systemctl daemon-reload
if [ -x /opt/scos-fabric/bin/memory-gateway ]; then
  systemctl enable scos-fabric.target >/dev/null 2>&1
  # The migrations unit is a RemainAfterExit oneshot, so restarting the target
  # alone never re-runs it; apply new migrations before services restart.
  systemctl restart scos-fabric-migrations.service
  systemctl restart scos-fabric.target
  systemctl enable --now scos-index-maintenance-worker.timer scos-policy-evaluation-worker.timer >/dev/null 2>&1
  echo "==> started scos-fabric.target"
else
  echo "==> binaries missing from /opt/scos-fabric/bin -- run deploy/native/pi/push.sh from a build host"
fi
