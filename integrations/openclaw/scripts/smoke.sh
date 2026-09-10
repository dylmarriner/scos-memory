#!/usr/bin/env bash
# Live smoke test for the SCOS <-> OpenClaw integration.
# Verifies mcp-gateway is reachable and the tools the plugin calls answer.
# Read-only; makes no memory writes.
set -uo pipefail

ENV_FILE="${SCOS_ENV_FILE:-$HOME/.hermes/scos.env}"
[ -f "$ENV_FILE" ] && { set -a; . "$ENV_FILE"; set +a; }

URL="${SCOS_GATEWAY_URL:-https://127.0.0.1:8098/mcp}"
AGENT="${SCOS_AGENT_NAME:-openclaw}"
TOKEN="${SCOS_BEARER_TOKEN:-${SCOS_HERMES_BEARER_TOKEN:-}}"

hdr=(-H "Content-Type: application/json" -H "X-SCOS-Agent-Name: $AGENT")
[ -n "$TOKEN" ] && hdr+=(-H "Authorization: Bearer $TOKEN")
[ -n "${SCOS_TENANT_ID:-}" ] && hdr+=(-H "X-SCOS-Tenant-ID: $SCOS_TENANT_ID")
[ -n "${SCOS_PRINCIPAL_ID:-}" ] && hdr+=(-H "X-SCOS-Principal-ID: $SCOS_PRINCIPAL_ID")

call() {
  curl -sk -m 15 -X POST "$URL" "${hdr[@]}" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":$2}}"
}

echo "== SCOS OpenClaw smoke =="
echo "gateway: $URL (agent=$AGENT)"
rc=0

STATUS="$(call scos_status '{}')"
if python3 -c "import json,sys; json.loads(sys.argv[1])['result']" "$STATUS" 2>/dev/null; then
  echo "PASS  scos_status reachable"
else
  echo "FAIL  scos_status"; rc=1
fi

RECALL="$(call scos_recall '{"query":"Dylan style preferences","max_items":3}')"
COUNT="$(python3 -c "
import json,sys
d=json.loads(sys.argv[1])
p=json.loads(d['result']['content'][0]['text'])
print(len(p.get('recall') or []))
" "$RECALL" 2>/dev/null)"
if [ -n "$COUNT" ]; then
  echo "PASS  scos_recall returned $COUNT items"
else
  echo "FAIL  scos_recall"; rc=1
fi

SUMM="$(call scos_memory_summary '{"max_facts":"10"}')"
if python3 -c "import json,sys; json.loads(sys.argv[1])['result']['content'][0]['text']" "$SUMM" 2>/dev/null; then
  echo "PASS  scos_memory_summary reachable"
else
  echo "FAIL  scos_memory_summary"; rc=1
fi

echo
[ "$rc" -eq 0 ] && echo "SMOKE OK" || { echo "SMOKE FAILED"; exit 1; }
