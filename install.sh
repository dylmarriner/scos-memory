#!/usr/bin/env bash
# Thin bootstrap for the dashboard's "Connect this machine" one-liner:
#   curl -fsSL <origin>/install.sh | bash -s -- --token=<token> --base-url=<origin>
#
# scripts/cloud_provision.py and scripts/install_agent_plugins.py need the
# full repository tree (they copy from integrations/agent-plugins/,
# integrations/hermes/, integrations/openclaw/skills/, etc.), not just
# themselves -- so on a machine that doesn't already have a checkout, this
# clones one to a local cache directory rather than trying to hand-fetch
# every file individually.
set -euo pipefail

REPO_URL="${SCOS_REPO_GIT_URL:-https://github.com/dylmarriner/scos-memory.git}"
REPO_CACHE="${SCOS_REPO_CACHE_DIR:-$HOME/.hermes/scos-repo}"
TOKEN=""
BASE_URL=""

for arg in "$@"; do
  case "$arg" in
    --token=*) TOKEN="${arg#--token=}" ;;
    --base-url=*) BASE_URL="${arg#--base-url=}" ;;
    *) echo "unknown argument: $arg" >&2; exit 1 ;;
  esac
done

if [ -z "$TOKEN" ] || [ -z "$BASE_URL" ]; then
  echo "usage: install.sh --token=<token> --base-url=<origin>" >&2
  echo "(this is generated for you by the dashboard's Connect tab -- don't hand-type it)" >&2
  exit 1
fi

# Already inside a checkout (e.g. re-run from a repo you already have)?
# Reuse it instead of cloning a second copy.
if [ -f "scripts/cloud_provision.py" ] && [ -d "integrations" ]; then
  REPO_DIR="$(pwd)"
elif [ -d "$REPO_CACHE/.git" ]; then
  echo "updating cached checkout at $REPO_CACHE"
  git -C "$REPO_CACHE" pull --ff-only
  REPO_DIR="$REPO_CACHE"
else
  echo "cloning $REPO_URL -> $REPO_CACHE"
  git clone --depth 1 "$REPO_URL" "$REPO_CACHE"
  REPO_DIR="$REPO_CACHE"
fi

python3 "$REPO_DIR/scripts/cloud_provision.py" --token "$TOKEN" --base-url "$BASE_URL"
