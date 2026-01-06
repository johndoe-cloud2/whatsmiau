#!/bin/bash

# Script to update WhatsMiau application on AWS after code changes
# Usage: ./scripts/update-aws.sh
# This script builds, pushes, and deploys your code changes to AWS

set -e

AWS_REGION="${AWS_REGION:-us-east-1}"

echo "=========================================="
echo "Updating WhatsMiau on AWS"
echo "=========================================="
echo ""

# Check if AWS CLI is installed
if ! command -v aws &> /dev/null; then
    echo "ERROR: AWS CLI is not installed. Please install it first."
    exit 1
fi

# Check if Docker is installed
if ! command -v docker &> /dev/null; then
    echo "ERROR: Docker is not installed. Please install it first."
    exit 1
fi

# Check if terraform state exists
if [ ! -f "terraform/terraform.tfstate" ]; then
    echo "ERROR: Terraform state not found."
    echo "   Please run ./scripts/deploy-aws.sh first to create the infrastructure."
    exit 1
fi

# Get values from Terraform
echo "Getting infrastructure information from Terraform..."
cd terraform
ECR_REPO_URL=$(terraform output -raw ecr_repository_url 2>/dev/null || echo "")
CLUSTER_NAME=$(terraform output -raw cluster_name 2>/dev/null || echo "")
SERVICE_NAME=$(terraform output -raw service_name 2>/dev/null || echo "")
ALB_DNS=$(terraform output -raw alb_dns_name 2>/dev/null || echo "")
cd ..

if [ -z "$ECR_REPO_URL" ] || [ -z "$CLUSTER_NAME" ] || [ -z "$SERVICE_NAME" ]; then
    echo "ERROR: Could not get required information from Terraform."
    echo "   Make sure Terraform has been applied and infrastructure exists."
    exit 1
fi

echo "ECR Repository: $ECR_REPO_URL"
echo "ECS Cluster: $CLUSTER_NAME"
echo "ECS Service: $SERVICE_NAME"
echo ""

# Build Docker image
echo "Building Docker image..."
docker build -t $ECR_REPO_URL:latest .

# Login to ECR
echo "Logging in to ECR..."
aws ecr get-login-password --region $AWS_REGION | docker login --username AWS --password-stdin $ECR_REPO_URL

# Push image to ECR
echo "Pushing image to ECR..."
docker push $ECR_REPO_URL:latest

echo "Image pushed successfully"
echo ""

# Force ECS service update
echo "Updating ECS service to deploy new image..."
aws ecs update-service \
    --cluster $CLUSTER_NAME \
    --service $SERVICE_NAME \
    --force-new-deployment \
    --region $AWS_REGION > /dev/null

echo "Service update initiated"
echo ""

# Wait for service to stabilize (optional)
echo "Waiting for service to stabilize..."
echo "   (This may take a few minutes. Press Ctrl+C to skip waiting.)"
echo ""

if aws ecs wait services-stable \
    --cluster $CLUSTER_NAME \
    --services $SERVICE_NAME \
    --region $AWS_REGION 2>/dev/null; then
    echo "Service is stable and running with new image"
else
    echo "Service update is in progress. Check status in AWS Console."
fi

echo ""
echo "=========================================="
echo "Update completed!"
echo "=========================================="
echo ""
echo "Your application is available at: http://$ALB_DNS"
echo ""
echo "Monitor deployment:"
echo "  - ECS Console: https://console.aws.amazon.com/ecs"
echo "  - CloudWatch Logs: https://console.aws.amazon.com/cloudwatch"
echo ""
echo "To check service status:"
echo "  aws ecs describe-services --cluster $CLUSTER_NAME --services $SERVICE_NAME --region $AWS_REGION"

