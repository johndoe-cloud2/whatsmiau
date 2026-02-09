# Whatsmiau – local run, Docker, AWS infra and deploy
# For AWS: copy scripts/aws-config.env.example to scripts/aws-config.env and set API_SECRET, WEBHOOK_URL, ECR_REGISTRY (or AWS_ACCOUNT_ID).
# Use an AWS profile: make infra-create PROFILE=ases  or  make push-prod PROFILE=ases  (ases = name of profile in ~/.aws/credentials)

.PHONY: help run run-router local up down test-stack infra-create infra-update infra-destroy push-prod

# PROFILE=ases or AWS_PROFILE=ases; scripts use AWS_PROFILE for all aws CLI calls
AWS_PROFILE ?= $(PROFILE)
export AWS_PROFILE

help:
	@echo "Local:"
	@echo "  make run          - run backend (go run main.go)"
	@echo "  make run-router   - run router (go run ./cmd/router/)"
	@echo "  make up           - docker-compose up -d --build (router + backend + redis)"
	@echo "  make down         - docker-compose down"
	@echo "  make test-stack   - up + build, then run API tests (como en producción)"
	@echo "AWS infra (CloudFormation) – use PROFILE=nombre for a different profile:"
	@echo "  make infra-create [PROFILE=ases]   - create stack + ECR repos"
	@echo "  make infra-update [PROFILE=ases]  - update stack"
	@echo "  make infra-destroy [PROFILE=ases] - delete stack"
	@echo "  make push-prod [PROFILE=ases]      - build, push ECR, force ECS deploy"

run:
	go run main.go

run-router:
	PORT=8080 go run ./cmd/router/

up:
	docker-compose up -d --build

down:
	docker-compose down

test-stack:
	./scripts/test-stack.sh

infra-create:
	@[ -f scripts/aws-config.env ] || (echo "Copy scripts/aws-config.env.example to scripts/aws-config.env and set API_SECRET, WEBHOOK_URL, ECR_REGISTRY or AWS_ACCOUNT_ID" && exit 1)
	./scripts/aws-create-stack.sh

infra-update:
	./scripts/aws-update-stack.sh

infra-destroy:
	./scripts/aws-delete-stack.sh

push-prod:
	@[ -f scripts/aws-config.env ] || (echo "Copy scripts/aws-config.env.example to scripts/aws-config.env and set STACK_NAME, AWS_REGION, ECR_REGISTRY (or AWS_ACCOUNT_ID)" && exit 1)
	./scripts/aws-push-prod.sh
