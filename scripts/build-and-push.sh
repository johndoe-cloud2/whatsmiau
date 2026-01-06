#!/bin/bash

# Script to build and push Docker image to ECR
# Usage: ./scripts/build-and-push.sh [tag]

set -e

TAG="${1:-latest}"
AWS_REGION="${AWS_REGION:-us-east-1}"

# Get ECR URL from Terraform
if [ -f "terraform/terraform.tfstate" ]; then
    ECR_REPO_URL=$(cd terraform && terraform output -raw ecr_repository_url 2>/dev/null || echo "")
fi

if [ -z "$ECR_REPO_URL" ]; then
    echo "ERROR: Could not get ECR URL."
    echo "   Make sure Terraform has been applied first."
    exit 1
fi

echo "Building Docker image..."
docker build -t $ECR_REPO_URL:$TAG .

echo "Pushing image to ECR..."
aws ecr get-login-password --region $AWS_REGION | docker login --username AWS --password-stdin $ECR_REPO_URL
docker push $ECR_REPO_URL:$TAG

echo "Image pushed: $ECR_REPO_URL:$TAG"

# Force ECS service update
if command -v aws &> /dev/null && [ -f "terraform/terraform.tfstate" ]; then
    CLUSTER_NAME=$(cd terraform && terraform output -raw cluster_name 2>/dev/null || echo "")
    SERVICE_NAME=$(cd terraform && terraform output -raw service_name 2>/dev/null || echo "")
    
    if [ ! -z "$CLUSTER_NAME" ] && [ ! -z "$SERVICE_NAME" ]; then
        echo "Forcing ECS service update..."
        aws ecs update-service \
            --cluster $CLUSTER_NAME \
            --service $SERVICE_NAME \
            --force-new-deployment \
            --region $AWS_REGION > /dev/null 2>&1
        echo "Service updated. New containers will be deployed with the new image."
        echo ""
        echo "Note: The service update may take a few minutes to complete."
        echo "      Check status: aws ecs describe-services --cluster $CLUSTER_NAME --services $SERVICE_NAME --region $AWS_REGION"
    fi
fi

