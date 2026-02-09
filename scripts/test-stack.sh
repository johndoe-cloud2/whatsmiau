#!/usr/bin/env bash
# Levanta el stack igual que en producción (router + backend + redis) y ejecuta pruebas.
# Uso: ./scripts/test-stack.sh   o: make test-stack
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

API="http://localhost:8080"
SECRET="local-secret"
HEADER="X-Api-Secret"
INSTANCE="test-$(date +%s)"

echo "=== Build y levantado del stack (router + backend + redis) ==="
docker-compose up -d --build

echo "=== Esperando que router y backend respondan (máx 60s) ==="
for i in $(seq 1 30); do
  if curl -sS -o /dev/null -w "%{http_code}" -H "$HEADER: $SECRET" "$API/v1/instance" 2>/dev/null | grep -q 200; then
    echo "Router listo."
    break
  fi
  [ $i -eq 30 ] && (echo "Timeout: router no respondió."; docker-compose logs --tail=30; exit 1)
  sleep 2
done

echo ""
echo "=== 1. Listar instancias (debe ser [] o lista) ==="
curl -sS -H "$HEADER: $SECRET" "$API/v1/instance" | head -c 500
echo ""

echo ""
echo "=== 2. Crear instancia: $INSTANCE ==="
CREATE=$(curl -sS -w "\n%{http_code}" -X POST -H "$HEADER: $SECRET" -H "Content-Type: application/json" \
  -d "{\"instanceName\":\"$INSTANCE\"}" "$API/v1/instance")
HTTP=$(echo "$CREATE" | tail -n1)
BODY=$(echo "$CREATE" | sed '$d')
echo "HTTP $HTTP"
echo "$BODY" | head -c 400
echo ""

if [ "$HTTP" != "201" ]; then
  echo "Crear instancia falló (esperado 201). Logs:"
  docker-compose logs --tail=20
  exit 1
fi

echo ""
echo "=== 3. Estado de la instancia ==="
curl -sS -H "$HEADER: $SECRET" "$API/v1/instance/$INSTANCE/status" | head -c 300
echo ""

echo ""
echo "=== 4. Listar de nuevo (debe incluir $INSTANCE) ==="
curl -sS -H "$HEADER: $SECRET" "$API/v1/instance" | head -c 500
echo ""

echo ""
echo "=== Listo. Stack corriendo como en despliegue (router :8080, backend :8081 directo). ==="
echo "  Parar: docker-compose down"
echo "  Logs:  docker-compose logs -f"
