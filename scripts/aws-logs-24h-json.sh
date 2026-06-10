#!/usr/bin/env bash
set -e

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOGS_DIR="$REPO_ROOT/logs"
mkdir -p "$LOGS_DIR"

END_MS=$(($(date +%s) * 1000))
START_MS=$((END_MS - 24 * 3600 * 1000))
OUTPUT="$LOGS_DIR/cloudwatch-24h-$(date +%Y%m%d-%H%M%S).json"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

for profile in ases foxy; do
  env_file="$REPO_ROOT/.env.$profile"
  [ -f "$env_file" ] || { echo "Warning: $env_file not found, skipping." >&2; continue; }
  export AWS_PROFILE="$profile"
  source "$env_file"

  for svc in whatsmiau-router whatsmiau-backend; do
    echo "Fetching $profile / $svc..." >&2
    aws logs filter-log-events \
      --log-group-name "/ecs/${svc}-${STACK_NAME}" \
      --start-time "$START_MS" --end-time "$END_MS" \
      --region "${AWS_REGION:-us-east-1}" \
      --output json \
      --query 'events[*].{timestamp:timestamp,message:message,stream:logStreamName}' \
      > "$TMP/${profile}__${svc}.json" 2>/dev/null || echo "[]" > "$TMP/${profile}__${svc}.json"
  done
done

python3 - "$TMP" "$OUTPUT" << 'EOF'
import json, glob, sys, os
from datetime import datetime, timezone

tmp, out = sys.argv[1], sys.argv[2]
result = {"generated_at": datetime.now(timezone.utc).isoformat(), "hours": 24, "logs": {}}

for f in sorted(glob.glob(os.path.join(tmp, "*.json"))):
    profile, svc = os.path.basename(f).replace(".json", "").split("__", 1)
    result["logs"].setdefault(profile, {})[svc] = json.load(open(f))

json.dump(result, open(out, "w"), indent=2)
print(f"Saved: {out}", file=sys.stderr)
print(out)
EOF
