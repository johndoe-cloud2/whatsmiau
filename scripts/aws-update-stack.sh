#!/usr/bin/env bash
# Update CloudFormation stack (template or parameters).
# Loads: .env.production (repo root), then scripts/aws-config.env.
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CF_DIR="$REPO_ROOT/cloudformation"

[ -f "$REPO_ROOT/.env.production" ] && set -a && source "$REPO_ROOT/.env.production" && set +a
source "$SCRIPT_DIR/aws-config.env" 2>/dev/null || true
STACK_NAME="${STACK_NAME:-whatsmiau}"
AWS_REGION="${AWS_REGION:-us-east-1}"
[ -n "$AWS_PROFILE" ] && echo "Using AWS profile: $AWS_PROFILE"
ECR_REGISTRY="${ECR_REGISTRY:-}"
ECR_REPO_BACKEND="${ECR_REPO_BACKEND:-whatsmiau}"
ECR_REPO_ROUTER="${ECR_REPO_ROUTER:-whatsmiau-router}"
API_KEY="${API_KEY:-}"
WEBHOOK_URL="${WEBHOOK_URL:-}"

if [ -z "$ECR_REGISTRY" ]; then
  ECR_REGISTRY=$(aws sts get-caller-identity --query Account --output text 2>/dev/null)
  ECR_REGISTRY="${ECR_REGISTRY}.dkr.ecr.${AWS_REGION}.amazonaws.com"
fi
BACKEND_IMAGE="${ECR_REGISTRY}/${ECR_REPO_BACKEND}:latest"
ROUTER_IMAGE="${ECR_REGISTRY}/${ECR_REPO_ROUTER}:latest"

# Reuse current stack parameters if not set in env
if [ -z "$API_KEY" ] || [ -z "$WEBHOOK_URL" ]; then
  MAP=$(aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" --query 'Stacks[0].Parameters[*].[ParameterKey,ParameterValue]' --output text 2>/dev/null || true)
  [ -z "$API_KEY" ] && API_KEY=$(echo "$MAP" | awk '$1=="ApiKey"{print $2}')
  [ -z "$WEBHOOK_URL" ] && WEBHOOK_URL=$(echo "$MAP" | awk '$1=="WebhookURL"{print $2}')
fi
API_KEY="${API_KEY:-placeholder}"
WEBHOOK_URL="${WEBHOOK_URL:-https://example.com/webhook}"

PARAMS=(
  "ParameterKey=ApiKey,ParameterValue=$API_KEY"
  "ParameterKey=WebhookURL,ParameterValue=$WEBHOOK_URL"
  "ParameterKey=BackendImage,ParameterValue=$BACKEND_IMAGE"
  "ParameterKey=RouterImage,ParameterValue=$ROUTER_IMAGE"
)

echo "Updating stack: $STACK_NAME"
aws cloudformation update-stack \
  --stack-name "$STACK_NAME" \
  --template-body "file://$CF_DIR/template.yaml" \
  --parameters "${PARAMS[@]}" \
  --capabilities CAPABILITY_IAM \
  --region "$AWS_REGION" || true

# update-stack exits 255 when no updates; wait only if update was submitted
if aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" --query 'Stacks[0].StackStatus' --output text 2>/dev/null | grep -q UPDATE; then
  echo "Waiting for stack update..."
  aws cloudformation wait stack-update-complete --stack-name "$STACK_NAME" --region "$AWS_REGION"
fi
echo "Done. Outputs:"
aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" --query 'Stacks[0].Outputs' --output table
