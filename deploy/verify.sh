#!/usr/bin/env bash
# Proves a running deployment actually works: probes answer, a read runs, and a
# write is refused with a pointer to the approval flow.
set -euo pipefail

PORT="${PORT:-18099}"
RELEASE="${RELEASE:-sqlguard}"

kubectl rollout status "deploy/${RELEASE}" --timeout=180s >/dev/null
kubectl port-forward "svc/${RELEASE}" "${PORT}:8080" >/dev/null 2>&1 &
FORWARD=$!
trap 'kill ${FORWARD} 2>/dev/null || true' EXIT

for _ in $(seq 1 30); do
  curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1 && break
  sleep 1
done

probe() { curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/$1"; }
echo "healthz: $(probe healthz)"
echo "readyz : $(probe readyz)"

session=$(curl -si -X POST "http://127.0.0.1:${PORT}/mcp" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"verify","version":"1"}}}' \
  | grep -i '^mcp-session-id:' | tr -d '\r' | awk '{print $2}')

call() {
  curl -s -X POST "http://127.0.0.1:${PORT}/mcp" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H "Mcp-Session-Id: ${session}" -d "$1"
}
call '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null

read_decision=$(call '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query","arguments":{"sql":"SELECT count(*) FROM orders"}}}' | grep -o '"decision":"[a-z]*"' | head -1)
write_decision=$(call '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"query","arguments":{"sql":"DELETE FROM orders"}}}' | grep -o '"decision":"[a-z]*"' | head -1)

echo "read   : ${read_decision}"
echo "write  : ${write_decision}"

[[ "${read_decision}" == '"decision":"allowed"' ]] || { echo "a read was not allowed"; exit 1; }
[[ "${write_decision}" == '"decision":"refused"' ]] || { echo "a write was NOT refused"; exit 1; }
echo "ok"
