#!/usr/bin/env bash
# Test the *deployed* API (not local). Shows exactly what the router returns.
# Usage: PROFILE=ases ./scripts/aws-api-test.sh
#        PROFILE=foxy ./scripts/aws-api-test.sh
# Optional: API_BASE_URL=https://whatsmiau.asesadmin.com  (default by profile)
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/aws-config.env" 2>/dev/null || true
AWS_PROFILE="${PROFILE:-ases}"
export AWS_PROFILE
ENV_FILE="$REPO_ROOT/.env.$AWS_PROFILE"
[ ! -f "$ENV_FILE" ] && ENV_FILE="$REPO_ROOT/.env.production"
[ -f "$ENV_FILE" ] && set -a && source "$ENV_FILE" && set +a

# Base URL of the deployed API. Foxy has no cert on ALB by default → use HTTP; set API_BASE_URL=https://... if you use a TLS proxy.
if [ -z "$API_BASE_URL" ]; then
  case "$AWS_PROFILE" in
    ases)  API_BASE_URL="https://whatsmiau.asesadmin.com" ;;
    foxy)  API_BASE_URL="http://whatsmiau.foxyadminbot.info" ;;
    *)     API_BASE_URL="http://localhost:8080" ;;
  esac
fi

if [ -z "$API_KEY" ]; then
  echo "Error: API_KEY not set in $ENV_FILE. Same value is used by the router to validate the apikey header." >&2
  exit 1
fi

echo "=== Deployed API test (profile: $AWS_PROFILE) ==="
echo "  API_BASE_URL=$API_BASE_URL"
echo "  apikey header = (value from .env, length ${#API_KEY})"
echo ""

run() {
  local method="$1"
  local path="$2"
  local data="$3"
  local with_auth="$4"
  local url="${API_BASE_URL}${path}"
  local code
  local body
  # Do not follow redirects so we see the response from whatsmiau (not from api.asesadmin.com if it redirects)
  # Pass -H "apikey: $API_KEY" quoted so the full value (e.g. URL) is one header
  if [ -n "$data" ]; then
    if [ -n "$with_auth" ]; then
      body=$(curl -sS --max-redirs 0 -w "\n%{http_code}" -X "$method" -H "apikey: $API_KEY" -H "Content-Type: application/json" -d "$data" "$url" 2>/dev/null || true)
    else
      body=$(curl -sS --max-redirs 0 -w "\n%{http_code}" -X "$method" -H "Content-Type: application/json" -d "$data" "$url" 2>/dev/null || true)
    fi
  else
    if [ -n "$with_auth" ]; then
      body=$(curl -sS --max-redirs 0 -w "\n%{http_code}" -X "$method" -H "apikey: $API_KEY" "$url" 2>/dev/null || true)
    else
      body=$(curl -sS --max-redirs 0 -w "\n%{http_code}" -X "$method" "$url" 2>/dev/null || true)
    fi
  fi
  code=$(echo "$body" | tail -n1)
  body=$(echo "$body" | sed '$d')
  echo "  HTTP $code"
  echo "$body" | head -c 400
  [ ${#body} -gt 400 ] && echo "…"
  echo ""
  return 0
}

echo "--- 1. Health (no auth) ---"
run GET "/health" "" ""
echo "--- 2. List instances GET /v1/instance ---"
run GET "/v1/instance" "" "1"
echo "--- 3. Create instance POST /v1/instance ---"
run POST "/v1/instance" '{"instanceName":"test-diagnostic-'$(date +%s)'"}' "1"
echo "=== Done. If you see 503 = no backends in Redis. 502 = backend unreachable. 504 = ALB timeout. 401 = wrong apikey. ==="
