#!/usr/bin/env bash
# Update CloudFormation stack (template or parameters).
# Loads: .env.production (repo root), then scripts/aws-config.env.
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CF_DIR="$REPO_ROOT/cloudformation"

source "$SCRIPT_DIR/aws-config.env" 2>/dev/null || true
AWS_PROFILE="${AWS_PROFILE:-ases}"
export AWS_PROFILE
ENV_FILE="$REPO_ROOT/.env.$AWS_PROFILE"
[ ! -f "$ENV_FILE" ] && ENV_FILE="$REPO_ROOT/.env.production"
[ -f "$ENV_FILE" ] && set -a && source "$ENV_FILE" && set +a
[ -f "$SCRIPT_DIR/aws-infra.$AWS_PROFILE.env" ] && set -a && source "$SCRIPT_DIR/aws-infra.$AWS_PROFILE.env" && set +a
STACK_NAME="${STACK_NAME_OVERRIDE:-${STACK_NAME:-whatsmiau}}"
AWS_REGION="${AWS_REGION:-us-east-1}"
[ -n "$AWS_PROFILE" ] && echo "Using AWS profile: $AWS_PROFILE"
ECR_REGISTRY="${ECR_REGISTRY:-}"
ECR_REPO_BACKEND="${ECR_REPO_BACKEND:-whatsmiau}"
ECR_REPO_ROUTER="${ECR_REPO_ROUTER:-whatsmiau-router}"
API_KEY="${API_KEY:-}"
WEBHOOK_URL="${WEBHOOK_URL:-}"
DB_MIN_ACU="${DB_MIN_ACU:-}"
DB_MAX_ACU="${DB_MAX_ACU:-}"
DB_NAME="${DB_NAME:-}"
DB_USERNAME="${DB_USERNAME:-}"
DB_PASSWORD="${DB_PASSWORD:-}"

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
  [ -z "$DB_MIN_ACU" ] && DB_MIN_ACU=$(echo "$MAP" | awk '$1=="DatabaseMinACU"{print $2}')
  [ -z "$DB_MAX_ACU" ] && DB_MAX_ACU=$(echo "$MAP" | awk '$1=="DatabaseMaxACU"{print $2}')
  [ -z "$DB_NAME" ] && DB_NAME=$(echo "$MAP" | awk '$1=="DatabaseName"{print $2}')
  [ -z "$DB_USERNAME" ] && DB_USERNAME=$(echo "$MAP" | awk '$1=="DatabaseUsername"{print $2}')
  [ -z "$DB_PUBLIC_ACCESS" ] && DB_PUBLIC_ACCESS=$(echo "$MAP" | awk '$1=="DatabasePublicAccess"{print $2}')
fi
API_KEY="${API_KEY:-placeholder}"
WEBHOOK_URL="${WEBHOOK_URL:-https://example.com/webhook}"
CERTIFICATE_ARN="${CERTIFICATE_ARN:-}"
[ -z "$CERTIFICATE_ARN" ] && CERTIFICATE_ARN=""
DB_MIN_ACU="${DB_MIN_ACU:-0.5}"
DB_MAX_ACU="${DB_MAX_ACU:-4}"
DB_NAME="${DB_NAME:-whatsmiau}"
DB_USERNAME="${DB_USERNAME:-whatsmiau}"
DB_PUBLIC_ACCESS="${DB_PUBLIC_ACCESS:-false}"
HISTORY_SYNC_ENABLED="${HISTORY_SYNC_ENABLED:-true}"
HISTORY_SYNC_MAX_AGE_HOURS="${HISTORY_SYNC_MAX_AGE_HOURS:-24}"

if [ -n "$DB_PASSWORD" ]; then
  DB_PASSWORD_PARAM="{\"ParameterKey\":\"DatabasePassword\",\"ParameterValue\":\"$(echo "$DB_PASSWORD" | sed 's/"/\\"/g')\"}"
else
  DB_PASSWORD_PARAM='{"ParameterKey":"DatabasePassword","UsePreviousValue":true}'
fi

CF_PARAMS_FILE=$(mktemp)
trap "rm -f $CF_PARAMS_FILE" EXIT
cat <<EOF > "$CF_PARAMS_FILE"
[
  {"ParameterKey":"ApiKey","ParameterValue":"$(echo "$API_KEY" | sed 's/"/\\"/g')"},
  {"ParameterKey":"WebhookURL","ParameterValue":"$(echo "$WEBHOOK_URL" | sed 's/"/\\"/g')"},
  {"ParameterKey":"BackendImage","ParameterValue":"$BACKEND_IMAGE"},
  {"ParameterKey":"RouterImage","ParameterValue":"$ROUTER_IMAGE"},
  {"ParameterKey":"CertificateArn","ParameterValue":"$(echo "$CERTIFICATE_ARN" | sed 's/"/\\"/g')"},
  {"ParameterKey":"DatabaseMinACU","ParameterValue":"$DB_MIN_ACU"},
  {"ParameterKey":"DatabaseMaxACU","ParameterValue":"$DB_MAX_ACU"},
  {"ParameterKey":"DatabaseName","ParameterValue":"$DB_NAME"},
  {"ParameterKey":"DatabaseUsername","ParameterValue":"$DB_USERNAME"},
  {"ParameterKey":"DatabasePublicAccess","ParameterValue":"$DB_PUBLIC_ACCESS"},
  {"ParameterKey":"HistorySyncEnabled","ParameterValue":"$HISTORY_SYNC_ENABLED"},
  {"ParameterKey":"HistorySyncMaxAgeHours","ParameterValue":"$HISTORY_SYNC_MAX_AGE_HOURS"},
  $DB_PASSWORD_PARAM
]
EOF

echo "Updating stack: $STACK_NAME"
aws cloudformation update-stack \
  --stack-name "$STACK_NAME" \
  --template-body "file://$CF_DIR/template.yaml" \
  --parameters "file://$CF_PARAMS_FILE" \
  --capabilities CAPABILITY_IAM \
  --region "$AWS_REGION" || true

# update-stack exits 255 when no updates; wait only if update was submitted
if aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" --query 'Stacks[0].StackStatus' --output text 2>/dev/null | grep -q UPDATE; then
  echo "Waiting for stack update..."
  aws cloudformation wait stack-update-complete --stack-name "$STACK_NAME" --region "$AWS_REGION"
fi
echo "Done. Outputs:"
aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" --query 'Stacks[0].Outputs' --output table
