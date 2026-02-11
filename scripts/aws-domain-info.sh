#!/usr/bin/env bash
# Shows the ALB DNS name to configure CNAME in IONOS (external domain).
# Usage: AWS_PROFILE=ases ./scripts/aws-domain-info.sh
#        AWS_PROFILE=foxy ./scripts/aws-domain-info.sh
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/aws-config.env" 2>/dev/null || true
AWS_PROFILE="${AWS_PROFILE:-ases}"
export AWS_PROFILE
ENV_FILE="$REPO_ROOT/.env.$AWS_PROFILE"
[ ! -f "$ENV_FILE" ] && ENV_FILE="$REPO_ROOT/.env.production"
[ -f "$ENV_FILE" ] && set -a && source "$ENV_FILE" && set +a
STACK_NAME="${STACK_NAME:-whatsmiau}"
AWS_REGION="${AWS_REGION:-us-east-1}"

# Try ALBDNSName; if the stack is old and doesn't have it, use host from APIEndpoint
ALB_DNS=$(aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" \
  --query 'Stacks[0].Outputs[?OutputKey==`ALBDNSName`].OutputValue' --output text 2>/dev/null || true)
if [[ -z "$ALB_DNS" || "$ALB_DNS" != *".elb.amazonaws.com"* ]]; then
  API_URL=$(aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" \
    --query 'Stacks[0].Outputs[?OutputKey==`APIEndpoint`].OutputValue' --output text 2>/dev/null || true)
  if [[ "$API_URL" == http*://* ]]; then
    ALB_DNS="${API_URL#*://}"
    ALB_DNS="${ALB_DNS%%/*}"
  fi
fi
if [[ -z "$ALB_DNS" || "$ALB_DNS" != *".elb.amazonaws.com"* ]]; then
  echo "Could not get ALB for stack '$STACK_NAME' (region: $AWS_REGION, profile: $AWS_PROFILE)." >&2
  if ! aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" --profile "$AWS_PROFILE" &>/dev/null; then
    echo "  The stack does not exist. Create it with: make infra-create PROFILE=$AWS_PROFILE" >&2
  else
    echo "  The stack has no ALBDNSName or APIEndpoint output. Update with: make infra-update PROFILE=$AWS_PROFILE" >&2
  fi
  exit 1
fi

# Suggested domain by profile (domain in IONOS, not in AWS)
case "$AWS_PROFILE" in
  ases)  SUGGESTED_DOMAIN="whatsmiau.asesadmin.com" ;;
  foxy)  SUGGESTED_DOMAIN="whatsmiau.foxyadminbot.info" ;;
  *)     SUGGESTED_DOMAIN="whatsmiau.<your-domain-in-ionos>" ;;
esac

echo "Profile: $AWS_PROFILE"
echo "Stack:   $STACK_NAME"
echo ""
echo "--- Values to configure the domain in IONOS ---"
echo ""
echo "In IONOS create a CNAME record:"
echo "  Name (subdomain):  whatsmiau   (result: $SUGGESTED_DOMAIN)"
echo "  Target / Value:    $ALB_DNS"
echo ""
echo "API URL (when DNS is active): http://$SUGGESTED_DOMAIN"
echo ""
