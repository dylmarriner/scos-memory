#!/usr/bin/env bash
# One-time (idempotent) native install: system user, state directories, dev
# TLS certs, systemd units, and env file templates. Run as root. Does NOT
# build binaries (run build.sh first or after) and does NOT start anything --
# review /etc/scos/scos-fabric.env before starting the fleet.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root (sudo)" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
NATIVE_DIR="$REPO_ROOT/deploy/native"

echo "==> system user"
if ! getent group scos >/dev/null; then groupadd --system scos; fi
if ! id scos >/dev/null 2>&1; then
  useradd --system --gid scos --home-dir /var/lib/scos --no-create-home --shell /usr/sbin/nologin scos
fi

echo "==> state directories (/var/lib/scos/*, matches deploy/Dockerfile's layout)"
install -d -o scos -g scos -m 0750 \
  /var/lib/scos/agent-registry/state \
  /var/lib/scos/backups \
  /var/lib/scos/code-intelligence \
  /var/lib/scos/context \
  /var/lib/scos/control-plane \
  /var/lib/scos/execution-proxy/raw \
  /var/lib/scos/execution-proxy/state \
  /var/lib/scos/observability \
  /var/lib/scos/okf \
  /var/lib/scos/security-gate \
  /var/lib/scos/tool-governance \
  /var/lib/scos/wal-archive

# OKF content is Git-authoritative; the container image bakes a copy in for
# the same reason a native deploy uses a read-only bind: findOKFRoot()
# (services/okf-synchroniser/cmd/server/main.go) needs a writable directory
# to index into, and the source of truth should stay this checkout, not a
# second copy that can drift.
if [ ! -e /var/lib/scos/okf/.git ] && [ -d "$REPO_ROOT/okf" ]; then
  cp -a "$REPO_ROOT/okf/." /var/lib/scos/okf/
  chown -R scos:scos /var/lib/scos/okf
fi

echo "==> config directories"
install -d -o root -g scos -m 0750 /etc/scos/env
install -d -o root -g root -m 0755 /etc/scos/tls
install -d -o root -g root -m 0755 /etc/scos/tls-external

echo "==> internal dev TLS cert (SAN=localhost,127.0.0.1)"
if [ ! -f /etc/scos/tls/tls.crt ] || [ ! -f /etc/scos/tls/tls.key ]; then
  openssl req -x509 -nodes -newkey rsa:2048 -days 365 \
    -keyout /etc/scos/tls/tls.key -out /etc/scos/tls/tls.crt \
    -subj "/O=scos-internal-dev" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
  chown scos:scos /etc/scos/tls/tls.key
  chmod 600 /etc/scos/tls/tls.key
  chmod 644 /etc/scos/tls/tls.crt
  echo "    generated a self-signed development certificate"
else
  echo "    existing certificate found, leaving it in place"
fi
echo "    (mcp-gateway's external cert at /etc/scos/tls-external/ is NOT"
echo "     generated here -- see deploy/native/renew-mcp-gateway-cert.sh"
echo "     or provide a real certificate for that path yourself.)"

echo "==> database migration runner"
install -d -m 0755 /scos-database
ln -sfn "$REPO_ROOT/database/migrations" /scos-database/migrations
ln -sfn "$REPO_ROOT/database/policies" /scos-database/policies
install -m 0755 "$REPO_ROOT/scripts/scos-migrate" /usr/local/bin/scos-migrate

echo "==> env file templates (existing files are never overwritten)"
if [ ! -f /etc/scos/scos-fabric.env ]; then
  install -o root -g scos -m 0640 "$NATIVE_DIR/env/scos-fabric.env.example" /etc/scos/scos-fabric.env
  echo "    wrote /etc/scos/scos-fabric.env -- EDIT THIS before starting anything"
else
  echo "    /etc/scos/scos-fabric.env already exists, left unchanged"
fi
for f in "$NATIVE_DIR"/env/*.env; do
  name="$(basename "$f")"
  [ "$name" = "scos-fabric.env.example" ] && continue
  if [ ! -f "/etc/scos/env/$name" ]; then
    install -o root -g scos -m 0640 "$f" "/etc/scos/env/$name"
  fi
done

echo "==> systemd units"
install -m 0644 "$NATIVE_DIR"/systemd/*.service "$NATIVE_DIR"/systemd/*.target /etc/systemd/system/
systemctl daemon-reload

echo
echo "==> install complete. Next steps:"
echo "    1. Edit /etc/scos/scos-fabric.env (tokens, SCOS_DATABASE_URL, SCOS_AGENT_ENDPOINT)"
echo "    2. Run deploy/native/postgres-setup.sh (native PostgreSQL + pgvector + TLS)"
echo "    3. Run deploy/native/build.sh (builds binaries into /opt/scos-fabric)"
echo "    4. sudo systemctl start scos-fabric.target"
echo "    5. sudo systemctl enable scos-fabric.target   # to start on boot"
