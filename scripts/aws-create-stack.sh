#!/usr/bin/env bash
# Create CloudFormation stack and optionally ECR repos.
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
ECR_REGISTRY="${ECR_REGISTRY:?Set ECR_REGISTRY or AWS_ACCOUNT_ID in .env.production}"
ECR_REPO_BACKEND="${ECR_REPO_BACKEND:-whatsmiau}"
ECR_REPO_ROUTER="${ECR_REPO_ROUTER:-whatsmiau-router}"
API_KEY="${API_KEY:?Set API_KEY in .env.production}"
WEBHOOK_URL="${WEBHOOK_URL:?Set WEBHOOK_URL in .env.production}"

BACKEND_IMAGE="${ECR_REGISTRY}/${ECR_REPO_BACKEND}:latest"
ROUTER_IMAGE="${ECR_REGISTRY}/${ECR_REPO_ROUTER}:latest"

echo "Creating ECR repositories (if not exist)..."
aws ecr describe-repositories --repository-names "$ECR_REPO_BACKEND" --region "$AWS_REGION" 2>/dev/null || \
  aws ecr create-repository --repository-name "$ECR_REPO_BACKEND" --region "$AWS_REGION"
aws ecr describe-repositories --repository-names "$ECR_REPO_ROUTER" --region "$AWS_REGION" 2>/dev/null || \
  aws ecr create-repository --repository-name "$ECR_REPO_ROUTER" --region "$AWS_REGION"

echo "Creating CloudFormation stack: $STACK_NAME"
aws cloudformation create-stack \
  --stack-name "$STACK_NAME" \
  --template-body "file://$CF_DIR/template.yaml" \
  --parameters \
    "ParameterKey=ApiKey,ParameterValue=$API_KEY" \
    "ParameterKey=WebhookURL,ParameterValue=$WEBHOOK_URL" \
    "ParameterKey=BackendImage,ParameterValue=$BACKEND_IMAGE" \
    "ParameterKey=RouterImage,ParameterValue=$ROUTER_IMAGE" \
  --capabilities CAPABILITY_IAM \
  --region "$AWS_REGION"

echo "Waiting for stack create to complete..."
aws cloudformation wait stack-create-complete --stack-name "$STACK_NAME" --region "$AWS_REGION"
echo "Stack created. Outputs:"
aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" --query 'Stacks[0].Outputs' --output table
