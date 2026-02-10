#!/usr/bin/env bash
# Build both images, push to ECR, and force ECS services to deploy the new code.
# Loads: .env.production (repo root), then scripts/aws-config.env.
# Usage: ./scripts/aws-push-prod.sh   or: make push-prod
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
[ -f "$REPO_ROOT/.env.production" ] && set -a && source "$REPO_ROOT/.env.production" && set +a
source "$SCRIPT_DIR/aws-config.env" 2>/dev/null || true
[ -n "$AWS_PROFILE" ] && echo "Using AWS profile: $AWS_PROFILE"
STACK_NAME="${STACK_NAME:-whatsmiau}"
AWS_REGION="${AWS_REGION:-us-east-1}"
ECR_REGISTRY="${ECR_REGISTRY:-}"
ECR_REPO_BACKEND="${ECR_REPO_BACKEND:-whatsmiau}"
ECR_REPO_ROUTER="${ECR_REPO_ROUTER:-whatsmiau-router}"

if [ -z "$ECR_REGISTRY" ]; then
  AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text 2>/dev/null)
  ECR_REGISTRY="${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com"
fi
BACKEND_IMAGE="${ECR_REGISTRY}/${ECR_REPO_BACKEND}:latest"
ROUTER_IMAGE="${ECR_REGISTRY}/${ECR_REPO_ROUTER}:latest"
CLUSTER_NAME="whatsmiau-${STACK_NAME}"

# Fargate is linux/amd64 (required when building on arm64 e.g. Mac M1/M2)
echo "=== Build backend (linux/amd64) ==="
docker build --platform linux/amd64 -t "$BACKEND_IMAGE" -f "$REPO_ROOT/Dockerfile" "$REPO_ROOT"
echo "=== Build router (linux/amd64) ==="
docker build --platform linux/amd64 -t "$ROUTER_IMAGE" -f "$REPO_ROOT/Dockerfile.router" "$REPO_ROOT"

echo "=== Login to ECR ==="
aws ecr get-login-password --region "$AWS_REGION" | docker login --username AWS --password-stdin "$ECR_REGISTRY"

echo "=== Push images ==="
docker push "$BACKEND_IMAGE"
docker push "$ROUTER_IMAGE"

echo "=== Force ECS deployment (new tasks pull latest) ==="
aws ecs update-service --cluster "$CLUSTER_NAME" --service whatsmiau-backend  --force-new-deployment --region "$AWS_REGION" --query 'service.serviceName' --output text
aws ecs update-service --cluster "$CLUSTER_NAME" --service whatsmiau-router   --force-new-deployment --region "$AWS_REGION" --query 'service.serviceName' --output text

echo "=== Push prod done. New tasks are rolling out. ==="
echo "Check: aws ecs describe-services --cluster $CLUSTER_NAME --services whatsmiau-backend whatsmiau-router --region $AWS_REGION"
