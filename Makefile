# Whatsmiau – local run, Docker, AWS infra and deploy
# For AWS: copy .env.production.example to .env.production and set API_KEY, WEBHOOK_URL, ECR_REGISTRY (or AWS_ACCOUNT_ID).
# Optional: scripts/aws-config.env to override. Use PROFILE=name for AWS profile.

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
	@echo "  make test-stack   - up + build, then run API tests (same as production)"
	@echo "AWS infra (CloudFormation) – use PROFILE=name for a different profile:"
	@echo "  make infra-create [PROFILE=myprofile]   - create stack + ECR repos"
	@echo "  make infra-update [PROFILE=myprofile]   - update stack"
	@echo "  make infra-destroy [PROFILE=myprofile]  - delete stack"
	@echo "  make push-prod [PROFILE=myprofile]      - build, push ECR, force ECS deploy"

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
	@[ -f .env.production ] || (echo "Copy .env.production.example to .env.production and set API_KEY, WEBHOOK_URL, ECR_REGISTRY or AWS_ACCOUNT_ID" && exit 1)
	./scripts/aws-create-stack.sh

infra-update:
	./scripts/aws-update-stack.sh

infra-destroy:
	./scripts/aws-delete-stack.sh

push-prod:
	@[ -f .env.production ] || (echo "Copy .env.production.example to .env.production and set STACK_NAME, AWS_REGION, ECR_REGISTRY (or AWS_ACCOUNT_ID)" && exit 1)
	./scripts/aws-push-prod.sh
