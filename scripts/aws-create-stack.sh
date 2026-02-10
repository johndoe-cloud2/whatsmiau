#!/usr/bin/env bash
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CF_DIR="$REPO_ROOT/cloudformation"

ACTION="${1:-create}"
case "$ACTION" in
  create|destroy) ;;
  *)
    echo "Usage: $0 [create|destroy]"
    echo "  create  - Create ECR repos and CloudFormation stack (default)"
    echo "  destroy - Delete CloudFormation stack and ECR repos (full tear down)"
    exit 1
    ;;
esac

[ -f "$REPO_ROOT/.env.production" ] && set -a && source "$REPO_ROOT/.env.production" && set +a
source "$SCRIPT_DIR/aws-config.env" 2>/dev/null || true
AWS_PROFILE="${AWS_PROFILE:-ases}"
export AWS_PROFILE
STACK_NAME="${STACK_NAME:-whatsmiau}"
AWS_REGION="${AWS_REGION:-us-east-1}"
ECR_REPO_BACKEND="${ECR_REPO_BACKEND:-whatsmiau}"
ECR_REPO_ROUTER="${ECR_REPO_ROUTER:-whatsmiau-router}"
echo "Using AWS profile: $AWS_PROFILE"

if [ "$ACTION" = "create" ] && [ ! -f "$REPO_ROOT/.env.production" ]; then
  echo "Error: .env.production not found. Copy .env.production.example to .env.production and set API_KEY, WEBHOOK_URL (and optionally ECR_REGISTRY/AWS_ACCOUNT_ID)."
  exit 1
fi

if [ "$ACTION" = "destroy" ]; then
  echo "Destroying CloudFormation stack: $STACK_NAME"
  if aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" &>/dev/null; then
    aws cloudformation delete-stack --stack-name "$STACK_NAME" --region "$AWS_REGION"
    echo "Waiting for stack delete to complete..."
    aws cloudformation wait stack-delete-complete --stack-name "$STACK_NAME" --region "$AWS_REGION"
    echo "Stack destroyed."
  else
    echo "Stack '$STACK_NAME' does not exist or was already deleted."
  fi

  echo "Deleting ECR repositories..."
  for repo in "$ECR_REPO_BACKEND" "$ECR_REPO_ROUTER"; do
    if aws ecr describe-repositories --repository-names "$repo" --region "$AWS_REGION" &>/dev/null; then
      aws ecr delete-repository --repository-name "$repo" --region "$AWS_REGION" --force
      echo "Deleted ECR repo: $repo"
    else
      echo "ECR repo '$repo' does not exist, skipping."
    fi
  done
  echo "Destroy complete."
  exit 0
fi

if [ -z "$ECR_REGISTRY" ] && [ -n "$AWS_ACCOUNT_ID" ]; then
  ECR_REGISTRY="$AWS_ACCOUNT_ID.dkr.ecr.${AWS_REGION}.amazonaws.com"
fi
if [ -z "$ECR_REGISTRY" ]; then
  ECR_REGISTRY="$(aws sts get-caller-identity --query Account --output text 2>/dev/null).dkr.ecr.${AWS_REGION}.amazonaws.com"
fi
[ -n "$ECR_REGISTRY" ] || { echo "Error: Set ECR_REGISTRY or AWS_ACCOUNT_ID in .env.production, or ensure AWS profile has permissions."; exit 1; }
ECR_REPO_BACKEND="${ECR_REPO_BACKEND:-whatsmiau}"
ECR_REPO_ROUTER="${ECR_REPO_ROUTER:-whatsmiau-router}"
[ -n "$API_KEY" ] || { echo "Error: Set API_KEY in .env.production"; exit 1; }
[ -n "$WEBHOOK_URL" ] || { echo "Error: Set WEBHOOK_URL in .env.production"; exit 1; }

BACKEND_IMAGE="${ECR_REGISTRY}/${ECR_REPO_BACKEND}:latest"
ROUTER_IMAGE="${ECR_REGISTRY}/${ECR_REPO_ROUTER}:latest"

echo "Creating ECR repositories (if not exist)..."
aws ecr describe-repositories --repository-names "$ECR_REPO_BACKEND" --region "$AWS_REGION" 2>/dev/null || \
  aws ecr create-repository --repository-name "$ECR_REPO_BACKEND" --region "$AWS_REGION"
aws ecr describe-repositories --repository-names "$ECR_REPO_ROUTER" --region "$AWS_REGION" 2>/dev/null || \
  aws ecr create-repository --repository-name "$ECR_REPO_ROUTER" --region "$AWS_REGION"

echo "=== Build and push Docker images to ECR ==="
echo "Building backend..."
docker build -t "$BACKEND_IMAGE" -f "$REPO_ROOT/Dockerfile" "$REPO_ROOT"
echo "Building router..."
docker build -t "$ROUTER_IMAGE" -f "$REPO_ROOT/Dockerfile.router" "$REPO_ROOT"
echo "Login to ECR..."
aws ecr get-login-password --region "$AWS_REGION" | docker login --username AWS --password-stdin "$ECR_REGISTRY"
echo "Pushing images..."
docker push "$BACKEND_IMAGE"
docker push "$ROUTER_IMAGE"
echo "=== Images pushed ==="

# Siempre creamos VPC nueva + subnets en el template (100% aislado). Destroy borra todo.
echo "Creating new VPC and subnets (stack is fully isolated)"

CF_PARAMS_FILE=$(mktemp)
trap "rm -f $CF_PARAMS_FILE" EXIT
cat <<EOF > "$CF_PARAMS_FILE"
[
  {"ParameterKey":"ApiKey","ParameterValue":"$(echo "$API_KEY" | sed 's/"/\\"/g')"},
  {"ParameterKey":"WebhookURL","ParameterValue":"$(echo "$WEBHOOK_URL" | sed 's/"/\\"/g')"},
  {"ParameterKey":"BackendImage","ParameterValue":"$BACKEND_IMAGE"},
  {"ParameterKey":"RouterImage","ParameterValue":"$ROUTER_IMAGE"}
]
EOF

echo "Creating CloudFormation stack: $STACK_NAME"
aws cloudformation create-stack \
  --stack-name "$STACK_NAME" \
  --template-body "file://$CF_DIR/template.yaml" \
  --parameters "file://$CF_PARAMS_FILE" \
  --capabilities CAPABILITY_IAM \
  --region "$AWS_REGION"

echo "Waiting for stack create to complete..."
aws cloudformation wait stack-create-complete --stack-name "$STACK_NAME" --region "$AWS_REGION"
echo "Stack created. Outputs:"
aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" --query 'Stacks[0].Outputs' --output table
