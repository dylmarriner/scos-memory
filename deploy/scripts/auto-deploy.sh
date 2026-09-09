#!/usr/bin/env bash
# Polls origin/main and deploys new commits: pulls, rebuilds the shared
# scos-fabric image, the internal model-runtime image, and force-recreates
# services that need deterministic image refresh.
set -euo pipefail

REPO_DIR="${SCOS_REPO_DIR:-/opt/scos-memory-fabric}"
LOCK_FILE="/tmp/scos-auto-deploy.lock"

# The hosted Linode deployment uses the model-runtime overlay as part of the
# normal stack. BGE-M3 is the baseline model profile; reranker/code profiles
# can be enabled in .env with SCOS_MODEL_PROFILES=text-model,rerank-model,code-model.
export COMPOSE_PROFILES="${SCOS_MODEL_PROFILES:-text-model}"
COMPOSE=(docker compose --env-file .env \
  -f deploy/compose/docker-compose.yml \
  -f deploy/compose/docker-compose.models.yml)

FABRIC_SERVICES="agent-registry autonomous-control-plane backup-manager \
code-intelligence compatibility-gateway context-optimisation credential-vault execution-proxy \
cloud-control-plane extension-manager graphify-provider hermes-provider mcp-gateway memory-gateway \
memory-intelligence observability okf-synchroniser recovery-orchestrator \
retrieval-orchestrator security-gate tool-governance transparency-projection"

cd "$REPO_DIR"

BEFORE="$(git rev-parse HEAD)"
git fetch --quiet origin main
AFTER="$(git rev-parse origin/main)"

if [ "$BEFORE" = "$AFTER" ]; then
  exit 0
fi

exec 9>"$LOCK_FILE"
flock -n 9 || { echo "$(date -Iseconds) deploy already in progress, skipping this tick"; exit 0; }

echo "$(date -Iseconds) deploying $BEFORE -> $AFTER"
git merge --ff-only origin/main

"${COMPOSE[@]}" build \
  --build-arg "SOURCE_SHA=$AFTER" \
  --build-arg "BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Migrations are bind-mounted and the migrations container is one-shot, so a
# normal compose up cannot be trusted to re-run it after a new SQL file lands.
"${COMPOSE[@]}" run --rm migrations

# Shared fabric image services are force-recreated so they cannot quietly keep
# running the previous shared image after a successful build.
# shellcheck disable=SC2086
"${COMPOSE[@]}" up -d --no-deps --force-recreate $FABRIC_SERVICES

# model-runtime has its own image rather than the shared scos-fabric image.
# Recreate it explicitly too. Dependencies are allowed here so its internal
# certificate volume exists before the TLS listener starts.
"${COMPOSE[@]}" up -d --force-recreate model-runtime

# Bring up/update everything else, including the active model profile(s),
# embeddings-worker, tunnel-client, Postgres config changes, and optional
# services. This keeps BGE backends internal to the Compose network; no Nginx
# route or public model port is created by this deployment.
"${COMPOSE[@]}" up -d

# Verify the shared fabric-image services all run the image we just built.
EXPECTED_IMAGE="$(${COMPOSE[@]} images -q agent-registry)"
EXPECTED_IMAGE="${EXPECTED_IMAGE#sha256:}"
DRIFTED=""
for svc in $FABRIC_SERVICES; do
  container_id="$(${COMPOSE[@]} ps -q "$svc")"
  if [ -z "$container_id" ]; then
    echo "$(date -Iseconds) parity check: $svc has no running container"
    DRIFTED="$DRIFTED $svc"
    continue
  fi
  running_image="$(docker inspect --format '{{.Image}}' "$container_id")"
  running_image="${running_image#sha256:}"
  if [ "$running_image" != "$EXPECTED_IMAGE" ]; then
    echo "$(date -Iseconds) parity check: $svc is running image $running_image, expected $EXPECTED_IMAGE"
    DRIFTED="$DRIFTED $svc"
  fi
done

if [ -n "$DRIFTED" ]; then
  echo "$(date -Iseconds) deploy to $AFTER FAILED parity check:$DRIFTED"
  exit 1
fi

# Verify the internal model runtime is actually healthy after the BGE profile
# starts. This prevents a deploy from reporting success while embeddings are
# silently stuck behind an unavailable model backend.
MODEL_RUNTIME_ID="$(${COMPOSE[@]} ps -q model-runtime)"
if [ -z "$MODEL_RUNTIME_ID" ]; then
  echo "$(date -Iseconds) deploy to $AFTER FAILED: model-runtime has no running container"
  exit 1
fi
MODEL_RUNTIME_HEALTH="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$MODEL_RUNTIME_ID")"
if [ "$MODEL_RUNTIME_HEALTH" != "healthy" ]; then
  echo "$(date -Iseconds) deploy to $AFTER FAILED: model-runtime health=$MODEL_RUNTIME_HEALTH"
  exit 1
fi

echo "$(date -Iseconds) deploy complete: $(git rev-parse HEAD), fabric parity verified and model-runtime healthy"
