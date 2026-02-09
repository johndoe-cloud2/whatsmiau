#!/usr/bin/env bash
# Bring up the stack as in production (router + backend + redis) and run API tests.
# Usage: ./scripts/test-stack.sh   or: make test-stack
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

API="http://localhost:8080"
APIKEY="local-apikey"
HEADER="apikey"
INSTANCE="test-$(date +%s)"

# If router already responds, stack is up — skip build/up to avoid re-downloading
if curl -sS -o /dev/null -w "%{http_code}" -H "$HEADER: $APIKEY" "$API/v1/instance" 2>/dev/null | grep -q 200; then
  echo "=== Stack already up (router responding), skipping build/up ==="
else
  echo "=== Bring up stack (router + backend + redis) ==="
  docker-compose up -d --build
fi

echo "=== Waiting for router and backend (max 90s) ==="
for i in $(seq 1 45); do
  CODE=$(curl -sS -o /dev/null -w "%{http_code}" -H "$HEADER: $APIKEY" "$API/v1/instance" 2>/dev/null || echo "000")
  if [ "$CODE" = "200" ]; then
    echo "Router and backend ready."
    break
  fi
  if [ "$CODE" = "503" ]; then
    echo "  ... backend not registered yet (attempt $i/45)"
  fi
  [ $i -eq 45 ] && (echo "Timeout."; docker-compose logs --tail=40; exit 1)
  sleep 2
done

echo ""
echo "=== 1. List instances (expect [] or list) ==="
curl -sS -H "$HEADER: $APIKEY" "$API/v1/instance" | head -c 500
echo ""

echo ""
echo "=== 2. Create instance: $INSTANCE ==="
CREATE=$(curl -sS -w "\n%{http_code}" -X POST -H "$HEADER: $APIKEY" -H "Content-Type: application/json" \
  -d "{\"instanceName\":\"$INSTANCE\"}" "$API/v1/instance")
HTTP=$(echo "$CREATE" | tail -n1)
BODY=$(echo "$CREATE" | sed '$d')
echo "HTTP $HTTP"
echo "$BODY" | head -c 400
echo ""

if [ "$HTTP" != "201" ]; then
  echo "Create instance failed (expected 201). Logs:"
  docker-compose logs --tail=20
  exit 1
fi

echo ""
echo "=== 3. Instance status ==="
curl -sS -H "$HEADER: $APIKEY" "$API/v1/instance/$INSTANCE/status" | head -c 300
echo ""

echo ""
echo "=== 4. List again (should include $INSTANCE) ==="
curl -sS -H "$HEADER: $APIKEY" "$API/v1/instance" | head -c 500
echo ""

echo ""
echo "=== Done. API en http://localhost:8080 (router). Backend directo en :8081 solo para depurar. ==="
echo "  Stop: docker-compose down"
echo "  Logs: docker-compose logs -f"
