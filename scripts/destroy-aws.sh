#!/bin/bash

# Script to destroy WhatsMiau infrastructure on AWS
# Usage: ./scripts/destroy-aws.sh
# WARNING: This will delete ALL resources, including the database!

set -e

PROJECT_NAME="${PROJECT_NAME:-whatsmiau}"
AWS_REGION="${AWS_REGION:-us-east-1}"

echo "=========================================="
echo "WARNING: This will destroy ALL infrastructure!"
echo "=========================================="
echo ""
echo "This will permanently delete:"
echo "  - VPC and all networking (subnets, gateways, routes)"
echo "  - RDS PostgreSQL database (ALL DATA WILL BE LOST)"
echo "  - ElastiCache Redis (ALL DATA WILL BE LOST)"
echo "  - ECS cluster and services"
echo "  - Application Load Balancer"
echo "  - ECR repository (Docker images)"
echo "  - CloudWatch Log Groups"
echo "  - Security Groups"
echo "  - All other resources created by Terraform"
echo ""
echo "This action CANNOT be undone!"
echo "=========================================="
echo ""

read -p "Are you absolutely sure you want to continue? Type 'yes' to confirm: " -r
echo
if [[ ! $REPLY == "yes" ]]; then
    echo "Destruction cancelled."
    exit 1
fi

# Navigate to terraform directory
cd terraform

# Check if terraform is initialized
if [ ! -d ".terraform" ]; then
    echo "Initializing Terraform..."
    terraform init
fi

# Check if state exists
if [ ! -f "terraform.tfstate" ] && [ ! -f ".terraform/terraform.tfstate" ]; then
    echo "ERROR: No Terraform state found."
    echo "   Nothing to destroy. Infrastructure may not exist or was already destroyed."
    exit 1
fi

# Destroy infrastructure
echo ""
echo "Destroying infrastructure..."
echo "This may take 5-15 minutes depending on resources..."
echo ""

terraform destroy -auto-approve

echo ""
echo "=========================================="
echo "Infrastructure destroyed successfully!"
echo "=========================================="
echo ""
echo "All resources have been removed from AWS:"
echo "  - VPC and networking"
echo "  - RDS PostgreSQL database"
echo "  - ElastiCache Redis"
echo "  - ECS cluster and services"
echo "  - Application Load Balancer"
echo "  - ECR repository"
echo "  - CloudWatch Log Groups"
echo "  - Security Groups"
echo "  - All other resources"
echo ""
echo "Note: If you see any errors about resources not found,"
echo "      they may have already been deleted or never existed."
echo ""
echo "To verify, check your AWS Console or run:"
echo "  aws ecs list-clusters --region $AWS_REGION"
echo "  aws rds describe-db-instances --region $AWS_REGION"
