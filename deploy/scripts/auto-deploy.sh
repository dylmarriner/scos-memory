#!/usr/bin/env bash
# Polls origin/main and deploys new commits: pulls, rebuilds the shared
# scos-fabric image, and force-recreates every service built from it.
#
# Force-recreate is deliberate, not automatic compose diffing: on 2026-08-30
# six of twenty-two fabric services (compatibility-gateway, backup-manager,
# execution-proxy, context-optimisation, retrieval-orchestrator,
# memory-gateway) were left running a stale image for 42+ hours after a
# rebuild because whatever deployed that build didn't recreate them. This
# script always force-recreates the full fabric-image service list on every
# run that has a new commit, so that specific failure mode can't recur
# regardless of whether compose's own change detection would have caught it.
# Keep FABRIC_SERVICES in sync with deploy/native/build.sh's SERVICES list.
#
# The BGE model overlay is opt-in through SCOS_MODEL_RUNTIME_ENABLED=true.
# This keeps ordinary production deploys behavior-compatible until the live
# host's model backend/profile has been explicitly provisioned.
#
# Safe to run frequently via a timer: exits early when origin/main hasn't
# moved AND no prior rollout is pending. The Git-private pending marker
# prevents failed post-checkout rollouts from being silently skipped. flock
# prevents overlapping runs if a deploy is still in progress.
set -euo pipefail

REPO_DIR="${SCOS_REPO_DIR:-/opt/scos-memory-fabric}"
LOCK_FILE="/tmp/scos-auto-deploy.lock"
MODEL_RUNTIME_ENABLED="${SCOS_MODEL_RUNTIME_ENABLED:-false}"

COMPOSE=(docker compose --env-file .env -f deploy/compose/docker-compose.yml)
if [ "$MODEL_RUNTIME_ENABLED" = "true" ]; then
  export COMPOSE_PROFILES="${SCOS_MODEL_PROFILES:-text-model}"
  COMPOSE+=( -f deploy/compose/docker-compose.models.yml )
fi

FABRIC_SERVICES="agent-registry autonomous-control-plane backup-manager \
code-intelligence compatibility-gateway context-optimisation credential-vault execution-proxy \
cloud-control-plane extension-manager graphify-provider hermes-provider mcp-gateway memory-gateway \
memory-intelligence observability okf-synchroniser recovery-orchestrator \
retrieval-orchestrator security-gate tool-governance transparency-projection"

cd "$REPO_DIR"

BEFORE="$(git rev-parse HEAD)"
git fetch --quiet origin main
AFTER="$(git rev-parse origin/main)"
# A checkout can advance before the post-checkout HBA gate or later rollout
# steps fail. Persist pending work in Git's private directory, not a tracked
# file, so a repaired retry cannot mistake HEAD == origin/main for success.
PENDING_DEPLOY="$(git rev-parse --git-path scos-pending-deploy)"

# Catch host bind-mount ACL/mode drift *even when main has not changed*.
# A UID 1000-owned 0600 pg_hba.conf once left PostgreSQL restarting 2,391
# times because the official image reads it as UID 999. Never fix it by
# changing ownership or widening permissions automatically during rollout.
bash "$REPO_DIR/deploy/scripts/verify_postgres_hba_access.sh" \
  "$REPO_DIR/deploy/compose/postgres/pg_hba.conf"

if [ "$BEFORE" = "$AFTER" ] && [ ! -f "$PENDING_DEPLOY" ]; then
  exit 0
fi

exec 9>"$LOCK_FILE"
flock -n 9 || { echo "$(date -Iseconds) deploy already in progress, skipping this tick"; exit 0; }

echo "$(date -Iseconds) deploying $BEFORE -> $AFTER"
# Record the pending target BEFORE moving HEAD. Keep it across failures in
# checkout, preflight, build, migration, recreation or parity verification.
PENDING_TMP="${PENDING_DEPLOY}.tmp.$$"
printf '%s\n' "$AFTER" > "$PENDING_TMP"
mv -f -- "$PENDING_TMP" "$PENDING_DEPLOY"
git merge --ff-only origin/main

# Fast-forward checkout may replace the HBA source inode and discard ACLs.
# Check the *new* file before any build, database migration or recreation.
bash "$REPO_DIR/deploy/scripts/verify_postgres_hba_access.sh" \
  "$REPO_DIR/deploy/compose/postgres/pg_hba.conf"

"${COMPOSE[@]}" build --build-arg "SOURCE_SHA=$AFTER" --build-arg "BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# `database/migrations` is bind-mounted into the migrations container from
# this checkout, not baked into its image (see docker-compose.yml) -- so a
# new .sql file landing on disk is not a service-definition change compose's
# own `up` will notice, and a one-shot container that already exited 0 stays
# exited. `up -d` alone would silently never re-run this container, so a new
# migration added to the repo would never apply here even though it applies
# on a fresh install. `run --rm` always executes it fresh; the runner itself
# (scripts/scos-migrate) tracks applied versions in scos_schema_migrations,
# so running it on every deploy is a no-op except for genuinely new files,
# and it runs before the fabric services below so new schema is present
# before app code that might depend on it starts.
"${COMPOSE[@]}" run --rm migrations

# On the first model-enabled deploy the client services need the private CA
# file before they can even construct their model-runtime HTTP clients. The
# certificate initializer is idempotent, so run it before force-recreating
# retrieval/code-intelligence; --no-deps below deliberately cannot do this.
if [ "$MODEL_RUNTIME_ENABLED" = "true" ]; then
  "${COMPOSE[@]}" up --no-deps model-runtime-tls-certs
fi

# shellcheck disable=SC2086
"${COMPOSE[@]}" up -d --no-deps --force-recreate $FABRIC_SERVICES

# model-runtime has its own image rather than the shared scos-fabric image.
# Only touch it when the live deployment explicitly opted into the overlay.
if [ "$MODEL_RUNTIME_ENABLED" = "true" ]; then
  "${COMPOSE[@]}" up -d --force-recreate model-runtime
fi

# Second pass with normal (non-force) semantics so anything else declared in
# the active compose set -- workers, tunnel-client, postgres config changes,
# and, when enabled, model profiles -- also lands without bouncing postgres
# on every deploy.
"${COMPOSE[@]}" up -d

# Verify parity instead of trusting that force-recreate landed everywhere:
# every fabric service shares one built image (see deploy/Dockerfile), so
# each running container's image ID must match what was just built. This is
# the actual check for the failure mode this script exists to prevent --
# force-recreate is a mechanism, not proof it worked. A drifted service is
# named loudly and the script exits non-zero so the systemd timer's own
# failure state surfaces it, rather than a container quietly running the
# previous commit until someone happens to notice.
# `docker compose images -q` prints the bare digest ("78bb4cdc...");
# `docker inspect --format '{{.Image}}'` prints it "sha256:"-prefixed. Both
# name the same image, but compared as strings they never match -- every
# service failed this check on 2026-09-01 with running_image and
# EXPECTED_IMAGE printed identically in the log, which is what actually
# gave this away. Stripping the prefix from both sides makes the
# comparison compare the digest, not incidental formatting.
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

if [ "$MODEL_RUNTIME_ENABLED" = "true" ]; then
  MODEL_RUNTIME_ID="$(${COMPOSE[@]} ps -q model-runtime)"
  if [ -z "$MODEL_RUNTIME_ID" ]; then
    echo "$(date -Iseconds) deploy to $AFTER FAILED: model-runtime has no running container"
    exit 1
  fi

  MODEL_RUNTIME_WAIT_SECONDS="${SCOS_MODEL_RUNTIME_DEPLOY_WAIT_SECONDS:-300}"
  DEADLINE=$((SECONDS + MODEL_RUNTIME_WAIT_SECONDS))
  while :; do
    MODEL_RUNTIME_HEALTH="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$MODEL_RUNTIME_ID")"
    if [ "$MODEL_RUNTIME_HEALTH" = "healthy" ]; then
      break
    fi
    if [ "$MODEL_RUNTIME_HEALTH" = "exited" ] || [ "$MODEL_RUNTIME_HEALTH" = "dead" ]; then
      echo "$(date -Iseconds) deploy to $AFTER FAILED: model-runtime state=$MODEL_RUNTIME_HEALTH"
      exit 1
    fi
    if [ "$SECONDS" -ge "$DEADLINE" ]; then
      echo "$(date -Iseconds) deploy to $AFTER FAILED: model-runtime did not become healthy within ${MODEL_RUNTIME_WAIT_SECONDS}s (last=$MODEL_RUNTIME_HEALTH)"
      exit 1
    fi
    sleep 5
  done
fi

# Only a fully verified rollout may clear the pending marker. A later timer
# invocation then correctly no-ops until a genuinely new revision appears.
rm -- "$PENDING_DEPLOY"

if [ "$MODEL_RUNTIME_ENABLED" = "true" ]; then
  echo "$(date -Iseconds) deploy complete: $(git rev-parse HEAD), fabric parity verified and model-runtime healthy"
else
  echo "$(date -Iseconds) deploy complete: $(git rev-parse HEAD), parity verified across$(printf ' %s' $FABRIC_SERVICES)"
fi
