#!/usr/bin/env bash
# Fetch CloudWatch logs de TODOS los servicios de esta API (router, backend y, si existe, Redis)
# en el perfil ASES, para las últimas N horas.
#
# Servicios en el stack: ECS router, ECS backend, ElastiCache Redis.
# Solo router y backend tienen log groups en CloudWatch por defecto. Redis (ElastiCache)
# no escribe en CloudWatch a menos que habilites "Log delivery" en la consola;
# si lo haces, el script intenta también /aws/elasticache/* para este stack.
#
# Usage: ./scripts/aws-logs-fetch.sh <hours>
# Example: ./scripts/aws-logs-fetch.sh 2    # last 2 hours
#          ./scripts/aws-logs-fetch.sh 24   # last 24 hours
#
# Requires: AWS CLI, .env.ases with STACK_NAME and AWS_REGION.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

HOURS="${1:?Usage: $0 <hours> (e.g. 2 or 24)}"
if ! [[ "$HOURS" =~ ^[0-9]+$ ]]; then
  echo "Error: hours must be a positive integer." >&2
  exit 1
fi

# Current time and start time in milliseconds (CloudWatch expects ms since epoch)
END_MS=$(($(date +%s) * 1000))
START_MS=$((END_MS - HOURS * 3600 * 1000))

PROFILES="${AWS_LOGS_PROFILES:-ases}"

# Log groups que crea el CloudFormation de esta API (ECS)
ECS_LOG_GROUPS="whatsmiau-router whatsmiau-backend"

# Prefijo opcional para Redis si en el futuro se habilita log delivery en ElastiCache
ELASTICACHE_LOG_PREFIX="/aws/elasticache"

for profile in $PROFILES; do
  env_file="$REPO_ROOT/.env.$profile"
  if [ ! -f "$env_file" ]; then
    echo "Warning: $env_file not found, skipping profile $profile." >&2
    continue
  fi
  export AWS_PROFILE="$profile"
  set -a
  source "$env_file"
  set +a
  STACK_NAME="${STACK_NAME:-whatsmiau}"
  AWS_REGION="${AWS_REGION:-us-east-1}"

  echo ""
  echo "========== LOGS — profile: $profile (last ${HOURS}h) — $STACK_NAME @ $AWS_REGION =========="
  echo ""

  # 1) Logs de los servicios ECS (router, backend)
  for log_name in $ECS_LOG_GROUPS; do
    LOG_GROUP="/ecs/${log_name}-${STACK_NAME}"
    echo "--- $log_name ($LOG_GROUP) ---"
    aws logs filter-log-events \
      --log-group-name "$LOG_GROUP" \
      --start-time "$START_MS" \
      --end-time "$END_MS" \
      --region "$AWS_REGION" \
      --output text \
      --query 'events[*].[timestamp,message]' \
      | while IFS=$'\t' read -r ts msg; do
        if [ -n "$ts" ]; then
          dt=$(date -r "$((ts/1000))" "+%Y-%m-%d %H:%M:%S" 2>/dev/null) || \
              dt=$(date -d "@$((ts/1000))" "+%Y-%m-%d %H:%M:%S" 2>/dev/null) || \
              dt="$ts"
          printf "[%s] %s\n" "$dt" "$msg"
        fi
      done
    echo ""
  done

  # 2) Log groups de ElastiCache (Redis) si existen para este stack
  while IFS= read -r -d '' LOG_GROUP; do
    [ -z "$LOG_GROUP" ] && continue
    name=$(basename "$LOG_GROUP" | sed 's/^ *//')
    echo "--- redis/elasticache ($name) ---"
    aws logs filter-log-events \
      --log-group-name "$LOG_GROUP" \
      --start-time "$START_MS" \
      --end-time "$END_MS" \
      --region "$AWS_REGION" \
      --output text \
      --query 'events[*].[timestamp,message]' \
      2>/dev/null | while IFS=$'\t' read -r ts msg; do
        if [ -n "$ts" ]; then
          dt=$(date -r "$((ts/1000))" "+%Y-%m-%d %H:%M:%S" 2>/dev/null) || \
              dt=$(date -d "@$((ts/1000))" "+%Y-%m-%d %H:%M:%S" 2>/dev/null) || \
              dt="$ts"
          printf "[%s] %s\n" "$dt" "$msg"
        fi
      done
    echo ""
  done < <(aws logs describe-log-groups --region "$AWS_REGION" \
    --log-group-name-prefix "$ELASTICACHE_LOG_PREFIX" \
    --query "logGroups[?contains(logGroupName, \`$STACK_NAME\`)].logGroupName" \
    --output text 2>/dev/null | tr '\t' '\n' | while read -r g; do
      g=$(echo "$g" | sed 's/^ *//;s/ *$//')
      [ -n "$g" ] && [ "$g" != "None" ] && printf '%s\0' "$g"
    done) || true
done

echo "Done. (start: ${START_MS}, end: ${END_MS})"
