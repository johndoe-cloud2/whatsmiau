#!/bin/sh
# For ECS: set BACKEND_PUBLIC_URL from container metadata so the router can route to this container.
set -e
if [ -n "${ECS_CONTAINER_METADATA_URI_V4}" ]; then
  IP=$(wget -qO- "${ECS_CONTAINER_METADATA_URI_V4}" 2>/dev/null | jq -r '.Networks[0].IPv4Addresses[0] // empty')
  if [ -n "$IP" ]; then
    PORT="${PORT:-8080}"
    export BACKEND_PUBLIC_URL="http://${IP}:${PORT}"
  fi
fi
exec ./whatsmiau
