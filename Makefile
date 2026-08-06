# Whatsmiau – local run, Docker, AWS infra and deploy
# AWS: profile ases uses .env.ases (copy from .env.ases.example).
# push-prod deploys to the ases profile using its .env file.

.PHONY: help run run-router stop local up down test-stack infra-create infra-update infra-destroy push-prod domain-info logs-fetch api-test

# PROFILE=ases for infra (create/update/destroy); push-prod uses .env.ases
AWS_PROFILE ?= $(PROFILE)
export AWS_PROFILE

help:
	@echo "Local:"
	@echo "  make run          - run backend (go run main.go)"
	@echo "  make stop         - stop local backend (port 8081)"
	@echo "  make run-router   - run router (go run ./cmd/router/)"
	@echo "  make up           - docker-compose up -d --build (router + backend + redis)"
	@echo "  make down         - docker-compose down"
	@echo "  make test-stack   - up + build, then run API tests (same as production)"
	@echo "AWS infra (CloudFormation) – PROFILE=ases:"
	@echo "  make infra-create [PROFILE=ases]   - create stack + ECR (uses .env.ases)"
	@echo "  make infra-update [PROFILE=ases]   - update stack"
	@echo "  make infra-destroy [PROFILE=ases]  - delete stack"
	@echo "  make push-prod    - build once, push and ECS deploy to ases (.env.ases)"
	@echo "  make domain-info  - show ALB DNS for the domain's CNAME (use PROFILE=ases)"
	@echo "  make logs-fetch HOURS=N - fetch CloudWatch logs from ases for the last N hours"
	@echo "  make logs-api-prod [HOURS=N] - fetch logs from API prod (api.asesadmin.com) that receives webhooks"
	@echo "  make logs-webhook [HOURS=N] - fetch logs from BOTH: WhatsMiau (sends) + API prod (receives)"
	@echo "  make api-test [PROFILE=ases] - test deployed API (health, list, create) to check 503/502/504"

# API on 8081 so 8080 is free for webhook URL locally. Uses .env and streams all logs to the terminal.
run:
	@[ -f .env ] || (echo ".env not found. Copy from .env.example." && exit 1)
	@echo "Using .env · API on :8081 · logs below:"
	PORT=8081 go run main.go

stop:
	@lsof -t -i:8081 | xargs kill 2>/dev/null || true
	@echo "Backend on 8081 stopped (or was not running)."

run-router:
	PORT=8080 go run ./cmd/router/

up:
	docker-compose up -d --build

down:
	docker-compose down

test-stack:
	./scripts/test-stack.sh

infra-create:
	@[ -f .env.ases ] || [ -f .env.production ] || (echo "Copy .env.ases.example to .env.ases (or .env.production) and set API_KEY, WEBHOOK_URL" && exit 1)
	./scripts/aws-create-stack.sh

infra-update:
	./scripts/aws-update-stack.sh

infra-destroy:
	./scripts/aws-delete-stack.sh

push-prod:
	@[ -f .env.ases ] || (echo "push-prod requires .env.ases (copy from .env.ases.example)" && exit 1)
	./scripts/aws-push-prod.sh

domain-info:
	./scripts/aws-domain-info.sh

# HOURS: number of hours back to fetch logs (router + backend in ases)
logs-fetch:
	@[ -n "$(HOURS)" ] || (echo "Usage: make logs-fetch HOURS=2  (or 24, etc.)" && exit 1)
	./scripts/aws-logs-fetch.sh "$(HOURS)"

# Logs de la API de prod (api.asesadmin.com) que recibe webhooks. Profile ases.
# Para ver si llegan los POST de WhatsMiau: make logs-api-prod  o  make logs-api-prod HOURS=2
logs-api-prod:
	./scripts/aws-logs-api-prod.sh $(if $(HOURS),$(HOURS),1)

# Logs de AMBOS: WhatsMiau (envía webhooks) + API prod (recibe). Debug completo del flujo.
logs-webhook:
	./scripts/aws-logs-webhook-full.sh $(if $(HOURS),$(HOURS),2)

# Test the deployed API (not local). See scripts/aws-api-test.sh. After running, make logs-fetch HOURS=1 to see backends_count.
api-test:
	./scripts/aws-api-test.sh
