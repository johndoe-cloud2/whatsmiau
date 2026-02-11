#!/usr/bin/env bash
# Build both images once, then push to ECR and force ECS deploy for each profile (ases, foxy).
# Uses .env.ases for ases and .env.foxy for foxy.
# Usage: ./scripts/aws-push-prod.sh   or: make push-prod
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/aws-config.env" 2>/dev/null || true

PROFILES="${AWS_PUSH_PROFILES:-ases foxy}"
LOCAL_BACKEND="whatsmiau-backend:build"
LOCAL_ROUTER="whatsmiau-router:build"

# Check that env files exist for each profile
for p in $PROFILES; do
  if [ ! -f "$REPO_ROOT/.env.$p" ]; then
    echo "Error: .env.$p not found. Copy .env.$p.example to .env.$p and fill in values." >&2
    exit 1
  fi
done

# Build once (linux/amd64 for Fargate)
echo '=== Build backend linux/amd64 ==='
docker build --platform linux/amd64 --no-cache -t "$LOCAL_BACKEND" -f "$REPO_ROOT/Dockerfile" "$REPO_ROOT"
echo '=== Build router linux/amd64 ==='
docker build --platform linux/amd64 --no-cache -t "$LOCAL_ROUTER" -f "$REPO_ROOT/Dockerfile.router" "$REPO_ROOT"

for AWS_PROFILE in $PROFILES; do
  export AWS_PROFILE
  echo ""
  echo "========== Deploying for profile: $AWS_PROFILE =========="
  # Reset ECR vars so each profile uses its own account (get-caller-identity uses current profile)
  unset ECR_REGISTRY
  set -a
  source "$REPO_ROOT/.env.$AWS_PROFILE"
  set +a
  STACK_NAME="${STACK_NAME:-whatsmiau}"
  AWS_REGION="${AWS_REGION:-us-east-1}"
  ECR_REPO_BACKEND="${ECR_REPO_BACKEND:-whatsmiau}"
  ECR_REPO_ROUTER="${ECR_REPO_ROUTER:-whatsmiau-router}"
  if [ -z "$ECR_REGISTRY" ]; then
    ECR_REGISTRY=$(aws sts get-caller-identity --query Account --output text 2>/dev/null)
    ECR_REGISTRY="${ECR_REGISTRY}.dkr.ecr.${AWS_REGION}.amazonaws.com"
  fi
  BACKEND_IMAGE="${ECR_REGISTRY}/${ECR_REPO_BACKEND}:latest"
  ROUTER_IMAGE="${ECR_REGISTRY}/${ECR_REPO_ROUTER}:latest"
  CLUSTER_NAME="whatsmiau-${STACK_NAME}"

  echo "=== Tag images for $AWS_PROFILE ==="
  docker tag "$LOCAL_BACKEND" "$BACKEND_IMAGE"
  docker tag "$LOCAL_ROUTER" "$ROUTER_IMAGE"

  echo "=== Login to ECR: $AWS_PROFILE ==="
  aws ecr get-login-password --region "$AWS_REGION" | docker login --username AWS --password-stdin "$ECR_REGISTRY"

  echo "=== Push images ==="
  docker push "$BACKEND_IMAGE"
  docker push "$ROUTER_IMAGE"

  echo "=== Force ECS deployment ==="
  aws ecs update-service --cluster "$CLUSTER_NAME" --service whatsmiau-backend  --force-new-deployment --region "$AWS_REGION" --query 'service.serviceName' --output text
  aws ecs update-service --cluster "$CLUSTER_NAME" --service whatsmiau-router   --force-new-deployment --region "$AWS_REGION" --query 'service.serviceName' --output text
done

echo ""
echo "=== Push prod done. Deployed to: $PROFILES ==="
echo "To verify: aws ecs describe-services --cluster whatsmiau-\$STACK_NAME --services whatsmiau-backend whatsmiau-router --region \$AWS_REGION with each profile"
