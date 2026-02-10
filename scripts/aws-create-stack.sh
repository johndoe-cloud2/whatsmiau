#!/usr/bin/env bash
# Create or destroy CloudFormation stack and ECR repos.
# Usage: $0 [create|destroy]   (default: create)
# Loads: .env.production (repo root), then scripts/aws-config.env.
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

# Optional: use existing VPC (set in .env.production, or auto-detect if not set).
# When auto-detecting we use subnets from a single AZ only (first AZ; if 1 subnet we use it twice).
VPC_ID="${VPC_ID:-}"
PUBLIC_SUBNET_IDS="${PUBLIC_SUBNET_IDS:-}"
PRIVATE_SUBNET_IDS="${PRIVATE_SUBNET_IDS:-}"
AUTO_VPC=""

# Subnets from one AZ when possible (first AZ); need 2 distinct subnets for ALB (no duplicates).
# If first AZ has 2+ subnets use those; if only 1, add first subnet from next AZ.
subnets_one_az() {
  local vpc_id=$1
  local all
  all=$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=$vpc_id" --query 'Subnets[*].[SubnetId,AvailabilityZone]' --output text --region "$AWS_REGION" 2>/dev/null | sort -k2)
  local first_az
  first_az=$(echo "$all" | head -1 | awk '{print $2}')
  local in_first_az
  in_first_az=$(echo "$all" | awk -v az="$first_az" '$2==az{print $1}')
  local n_first
  n_first=$(echo "$in_first_az" | grep -c . 2>/dev/null || echo 0)
  if [ "${n_first:-0}" -ge 2 ]; then
    echo "$in_first_az" | head -2 | paste -sd, -
  else
    # Need 2 distinct subnets (ALB rejects duplicates); take first two from any AZ
    echo "$all" | awk '{print $1}' | head -2 | paste -sd, -
  fi
}

# If VPC_ID is set but subnets missing, get subnets from that VPC (one AZ only)
if [ -n "$VPC_ID" ] && { [ -z "$PUBLIC_SUBNET_IDS" ] || [ -z "$PRIVATE_SUBNET_IDS" ]; }; then
  SUBNETS=$(subnets_one_az "$VPC_ID")
  if [ -n "$SUBNETS" ]; then
    [ -z "$PUBLIC_SUBNET_IDS" ] && PUBLIC_SUBNET_IDS="$SUBNETS"
    [ -z "$PRIVATE_SUBNET_IDS" ] && PRIVATE_SUBNET_IDS="$SUBNETS"
  fi
fi

# If still missing VPC or subnets, auto-detect: prefer default VPC, else first VPC with 1+ subnet(s)
if [ -z "$VPC_ID" ] || [ -z "$PUBLIC_SUBNET_IDS" ] || [ -z "$PRIVATE_SUBNET_IDS" ]; then
  DETECTED_VPC=""
  DEFAULT_VPC=$(aws ec2 describe-vpcs --filters "Name=isDefault,Values=true" --query 'Vpcs[0].VpcId' --output text --region "$AWS_REGION" 2>/dev/null || true)
  if [ -n "$DEFAULT_VPC" ] && [ "$DEFAULT_VPC" != "None" ]; then
    COUNT=$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=$DEFAULT_VPC" --query 'length(Subnets)' --output text --region "$AWS_REGION" 2>/dev/null || echo "0")
    [ "${COUNT:-0}" -ge 1 ] && DETECTED_VPC="$DEFAULT_VPC"
  fi
  if [ -z "$DETECTED_VPC" ]; then
    for vpc in $(aws ec2 describe-vpcs --query 'Vpcs[*].VpcId' --output text --region "$AWS_REGION" 2>/dev/null || true); do
      [ -z "$vpc" ] && continue
      COUNT=$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=$vpc" --query 'length(Subnets)' --output text --region "$AWS_REGION" 2>/dev/null || echo "0")
      if [ "${COUNT:-0}" -ge 1 ]; then DETECTED_VPC="$vpc"; break; fi
    done
  fi
  if [ -n "$DETECTED_VPC" ]; then
    SUBNETS=$(subnets_one_az "$DETECTED_VPC")
    if [ -n "$SUBNETS" ]; then
      [ -z "$VPC_ID" ] && VPC_ID="$DETECTED_VPC"
      [ -z "$PUBLIC_SUBNET_IDS" ] && PUBLIC_SUBNET_IDS="$SUBNETS"
      [ -z "$PRIVATE_SUBNET_IDS" ] && PRIVATE_SUBNET_IDS="$SUBNETS"
      AUTO_VPC=1
      echo "Using existing VPC (auto, single AZ): $VPC_ID (subnets: $PUBLIC_SUBNET_IDS)"
    fi
  fi
fi

if [ -n "$VPC_ID" ]; then
  [ -n "$PUBLIC_SUBNET_IDS" ] && [ -n "$PRIVATE_SUBNET_IDS" ] || { echo "Error: When VPC_ID is set, PUBLIC_SUBNET_IDS and PRIVATE_SUBNET_IDS are required (comma-separated subnet IDs)."; exit 1; }
  [ -z "$AUTO_VPC" ] && echo "Using existing VPC: $VPC_ID"
fi

# Use JSON file for parameters so comma in subnet IDs is preserved (CLI splits on comma otherwise)
CF_PARAMS_FILE=$(mktemp)
trap "rm -f $CF_PARAMS_FILE" EXIT
cat <<EOF > "$CF_PARAMS_FILE"
[
  {"ParameterKey":"ApiKey","ParameterValue":"$(echo "$API_KEY" | sed 's/"/\\"/g')"},
  {"ParameterKey":"WebhookURL","ParameterValue":"$(echo "$WEBHOOK_URL" | sed 's/"/\\"/g')"},
  {"ParameterKey":"BackendImage","ParameterValue":"$BACKEND_IMAGE"},
  {"ParameterKey":"RouterImage","ParameterValue":"$ROUTER_IMAGE"},
  {"ParameterKey":"VpcId","ParameterValue":"${VPC_ID:-}"},
  {"ParameterKey":"PublicSubnetIds","ParameterValue":"${PUBLIC_SUBNET_IDS:-}"},
  {"ParameterKey":"PrivateSubnetIds","ParameterValue":"${PRIVATE_SUBNET_IDS:-}"}
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
