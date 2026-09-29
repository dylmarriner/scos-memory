#!/bin/sh
# Native equivalent of deploy/scripts/renew-mcp-gateway-cert.sh: renews the
# Tailscale-issued (Let's Encrypt) TLS cert mcp-gateway serves to external
# MCP clients (Claude Code --transport http, Claude Desktop Connectors),
# which can't pin the internal self-signed dev CA the way
# tlsserve.TrustedClient can. `tailscale cert` is idempotent and only
# rewrites the files when renewal is actually due, so this is safe to run
# frequently via cron/systemd timer. Restarts the unit only when the cert
# actually changed, to avoid an unnecessary reconnect for every external MCP
# client.
set -eu

CERT_DIR="/etc/scos/tls-external"
DOMAIN="${SCOS_MCP_GATEWAY_TLS_DOMAIN:?set SCOS_MCP_GATEWAY_TLS_DOMAIN to your Tailscale MagicDNS name}"

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
install -m 640 -o root -g scos "$tmpdir/tls.key" "$CERT_DIR/tls.key"

systemctl restart scos-fabric@mcp-gateway.service

echo "$(date -Iseconds) renewed mcp-gateway cert ($BEFORE -> $AFTER) and restarted"
