#!/usr/bin/env bash
# Build on this (faster) machine, deploy to a Raspberry Pi over SSH:
#   deploy/native/pi/push.sh root@192.168.1.222
# Cross-compiles every pure-Go service for linux/arm64, then builds the one cgo
# service (code-intelligence, Tree-sitter) on the Pi itself with a pinned Go
# toolchain -- cross-compiling cgo here would link against this host's newer
# glibc. Binaries swap in atomically, then pi-setup.sh runs on the Pi.
set -euo pipefail

TARGET="${1:?usage: push.sh <user@pi-host>  (needs root or passwordless sudo)}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
REMOTE_SRC=/opt/scos-fabric/src
GO_VERSION="$(sed -n 's/^toolchain go//p' "$REPO_ROOT/go.mod")"
CGO_SERVICES="code-intelligence"
SERVICES="agent-registry autonomous-control-plane backup-manager \
code-intelligence compatibility-gateway context-optimisation credential-vault execution-proxy \
cloud-control-plane extension-manager graphify-provider hermes-provider mcp-gateway memory-gateway \
memory-intelligence observability okf-synchroniser recovery-orchestrator \
retrieval-orchestrator security-gate tool-governance transparency-projection"

OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT
SOURCE_SHA="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
LDFLAGS="-s -w -X github.com/dylmarriner/scos-memory/packages/release.SourceSHA=$SOURCE_SHA -X github.com/dylmarriner/scos-memory/packages/release.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "==> cross-compiling for linux/arm64"
cd "$REPO_ROOT"
for svc in $SERVICES; do
  case " $CGO_SERVICES " in *" $svc "*) continue ;; esac
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOFLAGS=-buildvcs=false \
    go build -trimpath -ldflags="$LDFLAGS" -o "$OUT/$svc" "./services/$svc/cmd/server"
done

echo "==> syncing source to $TARGET:$REMOTE_SRC"
ssh "$TARGET" "mkdir -p $REMOTE_SRC /opt/scos-fabric/bin.new"
rsync -az --delete --chown=root:root --chmod=D755,F644 --exclude .git --exclude node_modules --exclude _to_delete \
  --exclude audit-results --exclude /server --exclude __pycache__ --exclude .venv \
  "$REPO_ROOT/" "$TARGET:$REMOTE_SRC/"
# --chmod drops exec bits; restore them on scripts the Pi runs.
ssh "$TARGET" "find $REMOTE_SRC -name '*.sh' -exec chmod 755 {} + && chmod 755 $REMOTE_SRC/scripts/scos-migrate"
rsync -az --chown=root:root --chmod=D755,F755 "$OUT/" "$TARGET:/opt/scos-fabric/bin.new/"

echo "==> building cgo services on the Pi (low priority): $CGO_SERVICES"
ssh "$TARGET" "set -e
  TOOLS=/opt/scos-fabric/toolchain; GO=\$TOOLS/go/bin/go
  if [ \"\$(\$GO env GOVERSION 2>/dev/null)\" != go$GO_VERSION ]; then
    rm -rf \$TOOLS/go; mkdir -p \$TOOLS
    curl -fsSL https://go.dev/dl/go$GO_VERSION.linux-arm64.tar.gz | tar xz -C \$TOOLS
  fi
  cd $REMOTE_SRC
  for svc in $CGO_SERVICES; do
    CGO_ENABLED=1 GOFLAGS='-buildvcs=false -p=2' GOMAXPROCS=2 GOPATH=\$TOOLS/gopath GOCACHE=\$TOOLS/gocache \
      nice -n 19 ionice -c3 \$GO build -trimpath -ldflags='$LDFLAGS' -o /opt/scos-fabric/bin.new/\$svc ./services/\$svc/cmd/server
  done
  rm -rf /opt/scos-fabric/bin.old
  [ -d /opt/scos-fabric/bin ] && mv /opt/scos-fabric/bin /opt/scos-fabric/bin.old
  mv /opt/scos-fabric/bin.new /opt/scos-fabric/bin
  rm -rf /opt/scos-fabric/bin.old"

echo "==> pi-setup.sh"
ssh "$TARGET" "$REMOTE_SRC/deploy/native/pi/pi-setup.sh"
