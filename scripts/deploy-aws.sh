#!/bin/bash

# Script to deploy WhatsMiau on AWS
# Usage: ./scripts/deploy-aws.sh
# This script reads API_KEY and DB_PASSWORD from .env file in the project root

set -e

PROJECT_NAME="${PROJECT_NAME:-whatsmiau}"
AWS_REGION="${AWS_REGION:-us-east-1}"
ECR_REPO="${ECR_REPO:-whatsmiau}"

echo "Deploying WhatsMiau on AWS..."

# Check if terraform is installed
if ! command -v terraform &> /dev/null; then
    echo "ERROR: Terraform is not installed. Please install it first."
    exit 1
fi

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

# Load .env file from project root
ENV_FILE=".env"
if [ -f "$ENV_FILE" ]; then
    echo "Loading variables from .env file..."
    # Export variables from .env file (handles comments and empty lines)
    set -a
    source <(grep -v '^#' "$ENV_FILE" | grep -v '^$' | sed 's/^/export /')
    set +a
    
    # Extract DB_PASSWORD from DB_URL if DB_PASSWORD is not set
    if [ -z "$DB_PASSWORD" ] && [ -n "$DB_URL" ]; then
        # Try to extract password from postgres://user:password@host/db format
        if [[ "$DB_URL" =~ postgres://[^:]+:([^@]+)@ ]]; then
            DB_PASSWORD="${BASH_REMATCH[1]}"
            echo "Extracted DB_PASSWORD from DB_URL"
        fi
    fi
    
    # Set Terraform variables from .env (only API_KEY and DB_PASSWORD are needed)
    if [ -n "$API_KEY" ]; then
        export TF_VAR_api_key="$API_KEY"
        echo "API_KEY loaded from .env"
    else
        echo "WARNING: API_KEY not found in .env"
        echo "   Terraform requires API_KEY. Add it to your .env file."
    fi
    
    if [ -n "$DB_PASSWORD" ]; then
        export TF_VAR_db_password="$DB_PASSWORD"
        echo "DB_PASSWORD loaded from .env"
    else
        echo "WARNING: DB_PASSWORD not found in .env"
        echo "   Terraform requires DB_PASSWORD. Add it to your .env file:"
        echo "   DB_PASSWORD=your_password"
        echo "   Or extract it from DB_URL if it's in postgres://user:password@host/db format"
    fi
    
    echo ""
    echo "Note: Only API_KEY and DB_PASSWORD are used by Terraform."
    echo "      Other .env variables are for local development only."
else
    echo "WARNING: .env file not found. Using terraform.tfvars or environment variables."
fi

# Check if terraform.tfvars exists (optional now, but still useful for other variables)
if [ ! -f "terraform/terraform.tfvars" ]; then
    echo "INFO: terraform.tfvars does not exist. Using .env and environment variables."
    echo "   You can still create terraform.tfvars for other variables like aws_region, project_name, etc."
fi

# Navigate to terraform directory
cd terraform

# Initialize Terraform
echo "Initializing Terraform..."
terraform init

# Plan changes
echo "Planning infrastructure..."
terraform plan -out=tfplan

# Apply changes
echo "Creating infrastructure..."
read -p "Continue with deployment? (y/n) " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    terraform apply tfplan
else
    echo "Deployment cancelled."
    exit 1
fi

# Get outputs
ECR_REPO_URL=$(terraform output -raw ecr_repository_url)
ALB_DNS=$(terraform output -raw alb_dns_name)

echo "Infrastructure created!"
echo "ECR Repository: $ECR_REPO_URL"
echo "ALB DNS: $ALB_DNS"

# Return to root directory
cd ..

# Build and push Docker image
echo "Building Docker image..."
docker build -t $ECR_REPO_URL:latest .

echo "Pushing image to ECR..."
aws ecr get-login-password --region $AWS_REGION | docker login --username AWS --password-stdin $ECR_REPO_URL
docker push $ECR_REPO_URL:latest

echo "Deployment completed!"
echo ""
echo "Your application is available at: http://$ALB_DNS"
echo "Monitor in CloudWatch: https://console.aws.amazon.com/cloudwatch"
echo "ECS Cluster: https://console.aws.amazon.com/ecs"
