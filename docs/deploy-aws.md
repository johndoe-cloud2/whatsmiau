# Deploying Whatsmiau on AWS (single API with Router + ECS)

This describes the minimal AWS setup: one public API (Router) that validates the API key (header `apikey`) and routes requests to the correct ECS backend using Redis. Each backend runs the Whatsmiau API with SQLite and a single webhook URL; backends scale when CPU or memory exceeds 80%.

## Architecture

- **Router**: Validates header `apikey`, looks up Redis (`route:<instance_id>` or `backends` set), proxies HTTP to the right backend.
- **Redis**: Stores `route:<instance_id>` → backend URL and set `backends` (each backend registers on startup).
- **Backends**: Whatsmiau API; SQLite in `/app/data`; `WEBHOOK_URL` for all events; on session loss, instance and route are removed and `session.lost` is sent to the webhook.

## Environment variables

### Router (ECS task)

| Variable | Description |
|----------|-------------|
| `PORT` | Listen port (default 8080) |
| `REDIS_URL` | Redis host:port (e.g. from CloudFormation output) |
| `REDIS_PASSWORD` | Optional |
| `REDIS_TLS` | Set to true if Redis uses TLS |
| `API_KEY` | Value clients must send in the `apikey` header |

### Backend (ECS task)

| Variable | Description |
|----------|-------------|
| `PORT` | Listen port (default 8080) |
| `REDIS_URL` | Same Redis as router |
| `API_KEY` | Same key as router (validates `apikey` header) |
| `WEBHOOK_URL` | URL where all WhatsApp events are sent |
| `BACKEND_PUBLIC_URL` | Set automatically by entrypoint on ECS from task metadata (or set manually) |
| `DIALECT_DB` | `sqlite3` |
| `DB_URL` | `file:/app/data/data.db?_foreign_keys=on` |

## Deploy steps (scripts + Makefile)

Prerequisites: AWS CLI configured, Docker. Copy the production env and set your values:

```sh
cp .env.production.example .env.production
# Edit .env.production: API_KEY, WEBHOOK_URL, and either ECR_REGISTRY or AWS_ACCOUNT_ID (and optionally AWS_REGION, STACK_NAME, AWS_PROFILE)
```

**AWS profiles:** To use a profile from `~/.aws/credentials` (e.g. `ases`):

```sh
make infra-create PROFILE=ases
make push-prod PROFILE=ases
make infra-destroy PROFILE=ases
# Or set default in .env.production: AWS_PROFILE=ases
```

### 1. Create infrastructure (first time)

Creates ECR repos and the CloudFormation stack (VPC, Redis, ECS, ALB):

```sh
make infra-create
# or: ./scripts/aws-create-stack.sh
```

### 2. Build, push images and deploy new code (every time you change code)

Builds both images, pushes to ECR, and forces ECS to roll out the new tasks:

```sh
make push-prod
# or: ./scripts/aws-push-prod.sh
```

### 3. Update infrastructure (template or parameters)

After changing `cloudformation/template.yaml` or when you want to change parameters:

```sh
make infra-update
# or: ./scripts/aws-update-stack.sh
```

### 4. Delete stack

```sh
make infra-destroy
# or: ./scripts/aws-delete-stack.sh
```

### 5. Get the API endpoint

```sh
STACK_NAME=whatsmiau  # or your stack name
aws cloudformation describe-stacks --stack-name $STACK_NAME --query 'Stacks[0].Outputs[?OutputKey==`APIEndpoint`].OutputValue' --output text
```

Call the API with the API key in the header:

```sh
curl -H "apikey: YOUR_API_KEY" http://<APIEndpoint>/v1/instance
```

## Flow

1. Client sends a request to the Router (ALB) with header `apikey`.
2. Router checks the secret, then looks up Redis: for paths with an instance id (e.g. `/v1/instance/MYINSTANCE/...`) it uses `GET route:MYINSTANCE` to get the backend URL; for create/list it uses `SMEMBERS backends` and picks one backend.
3. Router proxies the request to that backend.
4. Backend handles the request; on session loss it deletes the instance and route from Redis and sends `session.lost` to `WEBHOOK_URL`.
