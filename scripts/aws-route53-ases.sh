#!/usr/bin/env bash
# Configure in Route 53 (AWS) the record whatsmiau.asesadmin.com -> ALB.
# The domain asesadmin.com is in the AWS account (Route 53), not in IONOS.
# Usage: AWS_PROFILE=ases ./scripts/aws-route53-ases.sh
set -e
AWS_PROFILE="${AWS_PROFILE:-ases}"
export AWS_PROFILE
AWS_REGION="${AWS_REGION:-us-east-1}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/aws-config.env" 2>/dev/null || true
[ -f "$REPO_ROOT/.env.ases" ] && set -a && source "$REPO_ROOT/.env.ases" && set +a
STACK_NAME="${STACK_NAME:-whatsmiau}"

# Get ALB DNS (and ALB hosted zone)
ALB_DNS=$(aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" \
  --query 'Stacks[0].Outputs[?OutputKey==`ALBDNSName`].OutputValue' --output text 2>/dev/null || true)
if [[ -z "$ALB_DNS" || "$ALB_DNS" != *".elb.amazonaws.com"* ]]; then
  API_URL=$(aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" \
    --query 'Stacks[0].Outputs[?OutputKey==`APIEndpoint`].OutputValue' --output text 2>/dev/null || true)
  [[ "$API_URL" == http*://* ]] && ALB_DNS="${API_URL#*://}" && ALB_DNS="${ALB_DNS%%/*}"
fi
if [[ -z "$ALB_DNS" || "$ALB_DNS" != *".elb.amazonaws.com"* ]]; then
  echo "Could not get ALB for stack $STACK_NAME." >&2
  exit 1
fi

ALB_ZONE=$(aws elbv2 describe-load-balancers --region "$AWS_REGION" \
  --query "LoadBalancers[?contains(DNSName, 'whatsmia')].CanonicalHostedZoneId | [0]" --output text 2>/dev/null || true)
if [[ -z "$ALB_ZONE" ]]; then
  ALB_ZONE="Z35SXDOTRQ7X7K"
fi

ZONE_ID=$(aws route53 list-hosted-zones --query "HostedZones[?Name=='asesadmin.com.'].Id" --output text 2>/dev/null | sed 's|/hostedzone/||')
if [[ -z "$ZONE_ID" ]]; then
  echo "Hosted zone asesadmin.com not found in Route 53." >&2
  exit 1
fi

echo "Configuring whatsmiau.asesadmin.com -> $ALB_DNS in Route 53 (zone $ZONE_ID)..."

aws route53 change-resource-record-sets --hosted-zone-id "$ZONE_ID" --change-batch "{
  \"Changes\": [{
    \"Action\": \"UPSERT\",
    \"ResourceRecordSet\": {
      \"Name\": \"whatsmiau.asesadmin.com\",
      \"Type\": \"A\",
      \"AliasTarget\": {
        \"HostedZoneId\": \"$ALB_ZONE\",
        \"DNSName\": \"$ALB_DNS\",
        \"EvaluateTargetHealth\": false
      }
    }
  }]
}" --output text --query 'ChangeInfo.Status'

echo "Done. The API will be at http://whatsmiau.asesadmin.com (may take 1–2 min to propagate)."
