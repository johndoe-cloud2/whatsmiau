#!/usr/bin/env bash
# Bring up the stack as in production (router + backend + redis) and verify API responds.
# Does not create new instances. Usage: ./scripts/test-stack.sh   or: make test-stack
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

API="http://localhost:8080"
APIKEY="local-apikey"
HEADER="apikey"

COMPOSE_FILES="-f docker-compose.yml -f docker-compose.test-stack.yml"

# En modo test, limpiar carpeta de medios de ejecuciones anteriores
if [ -d "media" ]; then
  find media -mindepth 1 -delete 2>/dev/null || true
  echo "=== Limpiada carpeta ./media ==="
fi

# If router already responds, stack is up — skip build/up to avoid re-downloading
if curl -sS -o /dev/null -w "%{http_code}" -H "$HEADER: $APIKEY" "$API/v1/instance" 2>/dev/null | grep -q 200; then
  echo "=== Stack already up (router responding), skipping build/up ==="
else
  echo "=== Bring up stack (router + backend + redis, test mode: media in ./media) ==="
  docker-compose $COMPOSE_FILES up -d --build
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
  [ $i -eq 45 ] && (echo "Timeout."; docker-compose $COMPOSE_FILES logs --tail=40; exit 1)
  sleep 2
done

echo ""
echo "=== List instances (verificación de API) ==="
CODE=$(curl -sS -o /dev/null -w "%{http_code}" -H "$HEADER: $APIKEY" "$API/v1/instance")
if [ "$CODE" != "200" ]; then
  echo "List instances failed (HTTP $CODE). Logs:"
  docker-compose $COMPOSE_FILES logs --tail=20
  exit 1
fi
curl -sS -H "$HEADER: $APIKEY" "$API/v1/instance" | head -c 500
echo ""

echo ""
echo "=== Done. API en http://localhost:8080 (router). No se crean instancias nuevas. Medios en ./media (solo en test-stack). ==="
echo "  Stop: docker-compose $COMPOSE_FILES down"
echo "  Logs: docker-compose $COMPOSE_FILES logs -f"
