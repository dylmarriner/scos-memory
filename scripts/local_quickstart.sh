#!/usr/bin/env bash
# Bring up a self-hosted, single-user SCOS Memory Fabric with mcp-gateway
# auth turned off entirely (SCOS_AUTH_MODE=local) -- no account, no bearer
# token, no tenant/principal to hand-provision. Every internal secret below
# is machine-generated plumbing between this fleet's own services; none of
# it is ever entered by a user or an agent.
#
# Deliberately excludes cloud-control-plane (the hosted accounts/billing
# product) and tunnel-client (remote exposure via OpenAI's control plane) --
# neither is needed, or wanted, for a local no-auth deployment. Nothing else
# depends on either service (see deploy/compose/docker-compose.yml), so
# omitting them from the `up` service list is sufficient.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_DIR="$ROOT/deploy/compose"
ENV_FILE="$COMPOSE_DIR/.env"
WORKSPACE_DIR="${SCOS_LOCAL_WORKSPACE_DIR:-$HOME/scos-local-workspaces}"

random_hex() { openssl rand -hex 32; }
random_b64_32() { openssl rand -base64 32; }

if [ -f "$ENV_FILE" ]; then
  backup="$ENV_FILE.bak-$(date -u +%Y%m%dT%H%M%SZ)"
  cp "$ENV_FILE" "$backup"
  echo "backed up existing $ENV_FILE -> $backup"
fi

mkdir -p "$WORKSPACE_DIR"

# Reuse secrets from a previous run: the postgres volume and credential vault
# keep the values they were initialised with, so regenerating them on a
# re-run would lock every service out of its own data.
existing() { if [ -f "$ENV_FILE" ]; then sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1; fi; }
POSTGRES_PASSWORD="$(existing POSTGRES_PASSWORD)"; POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-$(random_hex)}"
SCOS_APP_PASSWORD="$(existing SCOS_APP_PASSWORD)"; SCOS_APP_PASSWORD="${SCOS_APP_PASSWORD:-$(random_hex)}"
SCOS_WORKER_PASSWORD="$(existing SCOS_WORKER_PASSWORD)"; SCOS_WORKER_PASSWORD="${SCOS_WORKER_PASSWORD:-$(random_hex)}"
SCOS_BACKUP_PASSWORD="$(existing SCOS_BACKUP_PASSWORD)"; SCOS_BACKUP_PASSWORD="${SCOS_BACKUP_PASSWORD:-$(random_hex)}"
SCOS_INTERNAL_SERVICE_TOKEN="$(existing SCOS_INTERNAL_SERVICE_TOKEN)"; SCOS_INTERNAL_SERVICE_TOKEN="${SCOS_INTERNAL_SERVICE_TOKEN:-$(random_hex)}"
SCOS_CONTROL_PLANE_TOKEN="$(existing SCOS_CONTROL_PLANE_TOKEN)"; SCOS_CONTROL_PLANE_TOKEN="${SCOS_CONTROL_PLANE_TOKEN:-$(random_hex)}"
SCOS_SECURITY_ISSUER_SECRET="$(existing SCOS_SECURITY_ISSUER_SECRET)"; SCOS_SECURITY_ISSUER_SECRET="${SCOS_SECURITY_ISSUER_SECRET:-$(random_hex)}"
SCOS_CREDENTIAL_VAULT_MASTER_KEY="$(existing SCOS_CREDENTIAL_VAULT_MASTER_KEY)"; SCOS_CREDENTIAL_VAULT_MASTER_KEY="${SCOS_CREDENTIAL_VAULT_MASTER_KEY:-$(random_b64_32)}"
POSTGRES_DB="scos_memory"
POSTGRES_USER="scos"

cp "$ROOT/.env.example" "$ENV_FILE"

set_var() {
  local key="$1" value="$2"
  if grep -q "^${key}=" "$ENV_FILE"; then
    sed -i "s#^${key}=.*#${key}=${value}#" "$ENV_FILE"
  else
    printf '%s=%s\n' "$key" "$value" >> "$ENV_FILE"
  fi
}

set_var SCOS_TAILSCALE_IP 127.0.0.1
set_var POSTGRES_PASSWORD "$POSTGRES_PASSWORD"
set_var SCOS_APP_PASSWORD "$SCOS_APP_PASSWORD"
set_var SCOS_WORKER_PASSWORD "$SCOS_WORKER_PASSWORD"
set_var SCOS_BACKUP_PASSWORD "$SCOS_BACKUP_PASSWORD"
set_var SCOS_INTERNAL_SERVICE_TOKEN "$SCOS_INTERNAL_SERVICE_TOKEN"
set_var SCOS_CONTROL_PLANE_TOKEN "$SCOS_CONTROL_PLANE_TOKEN"
set_var SCOS_SECURITY_ISSUER_SECRET "$SCOS_SECURITY_ISSUER_SECRET"
set_var SCOS_CREDENTIAL_VAULT_MASTER_KEY "$SCOS_CREDENTIAL_VAULT_MASTER_KEY"
set_var SCOS_DATABASE_URL "postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=require"
set_var SCOS_APP_DATABASE_URL "postgres://scos_app:${SCOS_APP_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=require"
set_var SCOS_BACKUP_DATABASE_URL "postgres://scos_backup:${SCOS_BACKUP_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=require"
# The container's POSTGRES_USER is the cluster superuser, so it can create the
# temporary databases restore drills restore into.
set_var SCOS_BACKUP_RESTORE_DATABASE_URL "postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@postgres:5432/postgres?sslmode=require"
set_var SCOS_WORKER_DATABASE_URL "postgres://scos_worker:${SCOS_WORKER_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=require"
# Only consulted by autonomous-control-plane if it's ever asked to dispatch
# without DEEPSEEK_API_KEY set; compose still requires a non-empty value.
set_var SCOS_AGENT_ENDPOINT "disabled"
# cloud-control-plane is not started locally, but compose still interpolates
# its required variables when listing services.
set_var SCOS_PUBLIC_GATEWAY_URL "https://127.0.0.1:8098/mcp"
set_var GRAPHIFY_WORKSPACE_HOST_PATH "$WORKSPACE_DIR"
# tunnel-client is excluded from the `up` service list below (see comment at
# top), but `docker compose config`/`up` interpolate every service's
# environment up front regardless of which services actually start, so its
# required vars still need a placeholder even though nothing reads them.
set_var CONTROL_PLANE_API_KEY "unused-tunnel-client-disabled-in-local-mode"
set_var CONTROL_PLANE_TUNNEL_ID "unused-tunnel-client-disabled-in-local-mode"
set_var SCOS_GATEWAY_AUTHORIZATION "unused-tunnel-client-disabled-in-local-mode"

echo "wrote $ENV_FILE"

SERVICES="$(docker compose -f "$COMPOSE_DIR/docker-compose.yml" -f "$COMPOSE_DIR/docker-compose.local.yml" --env-file "$ENV_FILE" config --services | grep -v -e '^cloud-control-plane$' -e '^tunnel-client$')"

# shellcheck disable=SC2086
docker compose -f "$COMPOSE_DIR/docker-compose.yml" -f "$COMPOSE_DIR/docker-compose.local.yml" --env-file "$ENV_FILE" up -d $SERVICES

echo
echo "mcp-gateway is coming up at https://127.0.0.1:8098/mcp (self-signed cert; no auth required)."
echo "Connect an agent, e.g.:"
echo "  claude mcp add --transport http scos-local https://127.0.0.1:8098/mcp"
echo "or leave SCOS_BEARER_TOKEN/SCOS_TENANT_ID/SCOS_PRINCIPAL_ID unset in ~/.hermes/scos.env"
echo "and set SCOS_GATEWAY_URL=https://127.0.0.1:8098/mcp for the shared stdio bridge."
