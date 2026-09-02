#!/usr/bin/env bash
# Builds all 21 Go service binaries and the consolidation-worker Python
# package, without Docker. Mirrors deploy/Dockerfile's build stage exactly --
# same service list, same build flags -- so a native build produces the same
# binaries the container image would.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_BIN_DIR="${SCOS_NATIVE_BIN_DIR:-/opt/scos-fabric/bin}"
VENV_DIR="${SCOS_NATIVE_VENV_DIR:-/opt/scos-fabric/venv/consolidation}"

# Keep in sync with deploy/Dockerfile's SERVICES list. graphify-provider's
# binary builds here like any other service, but (unlike the Dockerfile) this
# script does not install the `graphify` CLI itself -- on a native box, run
# `pipx install graphifyy` (see github.com/Graphify-Labs/graphify) separately
# before starting graphify-provider.
SERVICES="agent-registry autonomous-control-plane backup-manager \
code-intelligence compatibility-gateway context-optimisation credential-vault execution-proxy \
cloud-control-plane extension-manager graphify-provider hermes-provider mcp-gateway memory-gateway \
memory-intelligence observability okf-synchroniser recovery-orchestrator \
retrieval-orchestrator security-gate tool-governance transparency-projection"

echo "==> building ${SERVICES// /, }"
cd "$REPO_ROOT"
mkdir -p "$INSTALL_BIN_DIR.new"
# CGO_ENABLED=1: code-intelligence's Tree-sitter parsers are cgo bindings and
# need a C toolchain (gcc/musl-dev or build-essential) on the build host --
# see deploy/Dockerfile for the matching container-build change.
export CGO_ENABLED=1 GOFLAGS=-buildvcs=false

# Stamped into packages/release's SourceSHA/BuildTime, mirroring
# deploy/Dockerfile's build-arg handling, so a native install reports the
# same GET /health/version provenance a container build would. Falls back to
# "unknown" outside a git checkout rather than failing the build.
SOURCE_SHA="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RELEASE_PKG="github.com/dylmarriner/scos-memory/packages/release"
LDFLAGS="-s -w -X $RELEASE_PKG.SourceSHA=$SOURCE_SHA -X $RELEASE_PKG.BuildTime=$BUILD_TIME"

for svc in $SERVICES; do
  echo "building $svc"
  go build -trimpath -ldflags="$LDFLAGS" -o "$INSTALL_BIN_DIR.new/$svc" "./services/$svc/cmd/server"
done

# Atomic swap: a build failure above never leaves a partially-overwritten
# bin/ directory that systemd could pick up mid-write.
rm -rf "$INSTALL_BIN_DIR.old"
[ -d "$INSTALL_BIN_DIR" ] && mv "$INSTALL_BIN_DIR" "$INSTALL_BIN_DIR.old"
mv "$INSTALL_BIN_DIR.new" "$INSTALL_BIN_DIR"
rm -rf "$INSTALL_BIN_DIR.old"

echo "==> building consolidation-worker (venv at $VENV_DIR)"
python3 -m venv "$VENV_DIR"
"$VENV_DIR/bin/pip" install --quiet --upgrade pip
"$VENV_DIR/bin/pip" install --quiet "$REPO_ROOT/workers/consolidation"

echo "==> done. Binaries in $INSTALL_BIN_DIR, worker venv in $VENV_DIR"
echo "    Run deploy/native/install.sh once (as root) before starting services,"
echo "    then re-run this script alone for subsequent rebuilds/upgrades and"
echo "    'sudo systemctl restart scos-fabric.target' to pick them up."
