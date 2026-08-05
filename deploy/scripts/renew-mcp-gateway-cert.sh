#!/bin/sh
# Renews the Tailscale-issued (Let's Encrypt) TLS cert mcp-gateway serves to
# external MCP clients (Claude Code --transport http, Claude Desktop
# Connectors), which can't pin the shared internal self-signed dev CA the
# way tlsserve.TrustedClient can. `tailscale cert` is idempotent and only
# rewrites the files when renewal is actually due, so this is safe to run
# frequently via cron. Restart mcp-gateway only when the cert actually
# changed, to avoid an unnecessary reconnect for every external MCP client.
set -eu

REPO_DIR="/opt/docker/scos-memory-fabric"
CERT_DIR="$REPO_DIR/tls-external"
DOMAIN="rabuntu.tail2c15c8.ts.net"

BEFORE=""
[ -f "$CERT_DIR/tls.crt" ] && BEFORE=$(sha256sum "$CERT_DIR/tls.crt" | cut -d' ' -f1)

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
tailscale cert --cert-file "$tmpdir/tls.crt" --key-file "$tmpdir/tls.key" "$DOMAIN"

AFTER=$(sha256sum "$tmpdir/tls.crt" | cut -d' ' -f1)
if [ "$BEFORE" = "$AFTER" ]; then
  exit 0
fi

install -m 644 -o root -g root "$tmpdir/tls.crt" "$CERT_DIR/tls.crt"
install -m 644 -o root -g root "$tmpdir/tls.key" "$CERT_DIR/tls.key"

cd "$REPO_DIR"
set -a
. ./.env
set +a
docker compose -p compose -f deploy/compose/docker-compose.yml -f deploy/compose/docker-compose.override.dev.yml \
  restart mcp-gateway

echo "$(date -Iseconds) renewed mcp-gateway cert ($BEFORE -> $AFTER) and restarted"
