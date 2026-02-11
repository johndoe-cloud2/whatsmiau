# Whatsmiau – local run, Docker, AWS infra and deploy
# AWS: perfil ases usa .env.ases, perfil foxy usa .env.foxy (copiar de .env.ases.example / .env.foxy.example).
# push-prod despliega en ambos perfiles (ases y foxy) usando sus .env.

.PHONY: help run run-router local up down test-stack infra-create infra-update infra-destroy push-prod domain-info logs-fetch api-test

# PROFILE=ases|foxy para infra (create/update/destroy); push-prod usa .env.ases y .env.foxy
AWS_PROFILE ?= $(PROFILE)
export AWS_PROFILE

help:
	@echo "Local:"
	@echo "  make run          - run backend (go run main.go)"
	@echo "  make run-router   - run router (go run ./cmd/router/)"
	@echo "  make up           - docker-compose up -d --build (router + backend + redis)"
	@echo "  make down         - docker-compose down"
	@echo "  make test-stack   - up + build, then run API tests (same as production)"
	@echo "AWS infra (CloudFormation) – PROFILE=ases o PROFILE=foxy:"
	@echo "  make infra-create [PROFILE=ases]   - create stack + ECR (usa .env.ases o .env.foxy)"
	@echo "  make infra-update [PROFILE=ases]   - update stack"
	@echo "  make infra-destroy [PROFILE=ases]  - delete stack"
	@echo "  make push-prod    - build once, push y ECS deploy en ases y foxy (.env.ases + .env.foxy)"
	@echo "  make domain-info  - muestra ALB DNS para CNAME en IONOS (usa PROFILE=ases o foxy)"
	@echo "  make logs-fetch HOURS=N - trae logs CloudWatch de ases y foxy últimas N horas"
	@echo "  make api-test [PROFILE=ases] - prueba API desplegado (health, list, create) para ver 503/502/504"

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
	@[ -f .env.ases ] || [ -f .env.foxy ] || [ -f .env.production ] || (echo "Copia .env.ases.example a .env.ases y/o .env.foxy.example a .env.foxy (o .env.production) y rellena API_KEY, WEBHOOK_URL" && exit 1)
	./scripts/aws-create-stack.sh

infra-update:
	./scripts/aws-update-stack.sh

infra-destroy:
	./scripts/aws-delete-stack.sh

push-prod:
	@[ -f .env.ases ] && [ -f .env.foxy ] || (echo "push-prod requiere .env.ases y .env.foxy (copia de .env.ases.example y .env.foxy.example)" && exit 1)
	./scripts/aws-push-prod.sh

domain-info:
	./scripts/aws-domain-info.sh

# HOURS: cantidad de horas hacia atrás para traer logs (router + backend en ases y foxy)
logs-fetch:
	@[ -n "$(HOURS)" ] || (echo "Uso: make logs-fetch HOURS=2  (o 24, etc.)" && exit 1)
	./scripts/aws-logs-fetch.sh "$(HOURS)"

# Prueba el API desplegado (no local). Ver scripts/aws-api-test.sh. Tras ejecutar, make logs-fetch HOURS=1 para ver backends_count.
api-test:
	./scripts/aws-api-test.sh
