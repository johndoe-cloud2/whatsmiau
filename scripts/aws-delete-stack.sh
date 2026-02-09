#!/usr/bin/env bash
# Delete CloudFormation stack. ECR repos and images are left as-is.
# Requires: AWS CLI, env loaded (source scripts/aws-config.env).
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/aws-config.env" 2>/dev/null || true
STACK_NAME="${STACK_NAME:-whatsmiau}"
AWS_REGION="${AWS_REGION:-us-east-1}"
[ -n "$AWS_PROFILE" ] && echo "Using AWS profile: $AWS_PROFILE"

echo "Deleting stack: $STACK_NAME"
aws cloudformation delete-stack --stack-name "$STACK_NAME" --region "$AWS_REGION"
echo "Waiting for stack delete..."
aws cloudformation wait stack-delete-complete --stack-name "$STACK_NAME" --region "$AWS_REGION" || true
echo "Stack deleted."
