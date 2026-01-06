# Terraform - AWS Infrastructure for WhatsMiau

This directory contains Terraform configuration to deploy WhatsMiau on AWS with autoscaling.

## Prerequisites

1. **AWS CLI** installed and configured
2. **Terraform** >= 1.0 installed
3. **Docker** installed
4. AWS credentials configured (`aws configure`)

## Initial Setup

### Option 1: Using .env file (Recommended)

The deployment script automatically reads `API_KEY` and `DB_PASSWORD` from your `.env` file in the project root.

1. Make sure you have a `.env` file in the project root:
   ```bash
   cp .env.example .env
   ```

2. Edit `.env` and set **only these two variables** (the rest are for local development only):
   - `API_KEY`: Your API key for authentication (required)
   - `DB_PASSWORD`: Strong password for PostgreSQL (required, or it will be extracted from `DB_URL` if in postgres format)

   Example `.env` entries:
   ```bash
   API_KEY=your_secure_api_key_here
   DB_PASSWORD=your_secure_password_here
   # Or use DB_URL with password:
   # DB_URL=postgres://username:password@host:5432/dbname?sslmode=require
   ```

   **Note**: Other variables in your `.env` (DEBUG_MODE, REDIS_PASSWORD, GCS_*, etc.) are only for local development and are NOT used by Terraform. In AWS, the application automatically receives the correct values from RDS and ElastiCache.

3. The script will automatically use these values for Terraform.

### Option 2: Using terraform.tfvars

1. Copy the example variables file:
   ```bash
   cp terraform.tfvars.example terraform.tfvars
   ```

2. Edit `terraform.tfvars` and fill in the values:
   - `db_password`: Strong password for PostgreSQL
   - `api_key`: API key for authentication

## Deployment

### Option 1: Automated Script (Recommended)

```bash
chmod +x ../scripts/deploy-aws.sh
../scripts/deploy-aws.sh
```

### Option 2: Manual

```bash
# Initialize Terraform
terraform init

# Review the plan
terraform plan

# Apply changes
terraform apply
```

## Infrastructure Structure

- **VPC**: Private network with public and private subnets
- **RDS PostgreSQL**: Shared database
- **ElastiCache Redis**: Shared cache
- **ECS Fargate**: Containers with autoscaling
- **Application Load Balancer**: Load balancer with sticky sessions
- **ECR**: Repository for Docker images

## Important Outputs

After deployment, Terraform will show:

- `alb_dns_name`: Load balancer URL (your application)
- `rds_endpoint`: Database endpoint
- `redis_endpoint`: Redis endpoint
- `ecr_repository_url`: ECR repository URL

## Updating the Application

After making code changes, use the update script:

### Option 1: Automated Script (Recommended)

```bash
./scripts/update-aws.sh
```

This script will:
1. Build your Docker image
2. Push it to ECR
3. Update the ECS service to deploy the new image
4. Wait for the service to stabilize

### Option 2: Using build-and-push.sh

```bash
./scripts/build-and-push.sh
```

### Option 3: Manual Steps

```bash
# Get ECR URL from Terraform
ECR_REPO_URL=$(cd terraform && terraform output -raw ecr_repository_url)

# Build and push
docker build -t $ECR_REPO_URL:latest .
aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin $ECR_REPO_URL
docker push $ECR_REPO_URL:latest

# Update ECS service
CLUSTER=$(cd terraform && terraform output -raw cluster_name)
SERVICE=$(cd terraform && terraform output -raw service_name)
aws ecs update-service --cluster $CLUSTER --service $SERVICE --force-new-deployment --region us-east-1
```

## Destroying Infrastructure

WARNING: This will delete all resources, including the database.

### Option 1: Automated Script (Recommended)

```bash
chmod +x ../scripts/destroy-aws.sh
../scripts/destroy-aws.sh
```

### Option 2: Manual

```bash
cd terraform
terraform destroy
```

**Important Notes:**
- All data in RDS and Redis will be permanently deleted
- The script requires you to type 'yes' to confirm
- This process cannot be undone
- Make sure you have backups if you need to preserve data

## Costs

Estimated monthly costs (us-east-1):

- ECS Fargate (1 task, 0.5 vCPU, 1GB): ~$8
- RDS PostgreSQL (db.t3.micro): ~$15
- ElastiCache Redis (cache.t3.micro): ~$12
- ALB: ~$20
- **Total**: ~$55/month (minimum)

## Troubleshooting

### Error: "Cannot connect to RDS"
- Verify security groups allow traffic from ECS
- Verify RDS is in private subnets

### Error: "Cannot connect to Redis"
- Verify ElastiCache is in the same VPC
- Verify security groups

### ECS tasks not starting
- Check logs in CloudWatch
- Verify Docker image is in ECR
- Verify environment variables are correct

## Monitoring

- **CloudWatch Logs**: `/ecs/whatsmiau`
- **CloudWatch Metrics**: ECS, RDS, Redis metrics
- **ECS Console**: Task and service status
