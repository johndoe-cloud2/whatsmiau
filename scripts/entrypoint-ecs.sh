#!/bin/sh
# For ECS: set BACKEND_PUBLIC_URL from container metadata so the router can route to this container.
set -e
if [ -n "${ECS_CONTAINER_METADATA_URI_V4}" ]; then
  RAW=$(wget -qO- "${ECS_CONTAINER_METADATA_URI_V4}/task" 2>/dev/null || true)
  if [ -n "$RAW" ]; then
    IP=$(echo "$RAW" | jq -r '.Containers[0].Networks[0].IPv4Addresses[0] // .Networks[0].IPv4Addresses[0] // empty' 2>/dev/null || true)
  fi
  if [ -z "$IP" ] && [ -n "${ECS_CONTAINER_METADATA_URI_V4}" ]; then
    RAW=$(wget -qO- "${ECS_CONTAINER_METADATA_URI_V4}" 2>/dev/null || true)
    IP=$(echo "$RAW" | jq -r '.Networks[0].IPv4Addresses[0] // empty' 2>/dev/null || true)
  fi
  if [ -n "$IP" ]; then
    PORT="${PORT:-8080}"
    export BACKEND_PUBLIC_URL="http://${IP}:${PORT}"
    echo "BACKEND_PUBLIC_URL=$BACKEND_PUBLIC_URL" >&2
  else
    echo "WARN: could not get task IP from ECS metadata (BACKEND_PUBLIC_URL will be unset)" >&2
  fi
fi
exec ./whatsmiau
