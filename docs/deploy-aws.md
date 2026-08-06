# Deploying Whatsmiau on AWS (single API with Router + ECS)

This describes the minimal AWS setup: one public API (Router) that validates the API key (header `apikey`) and routes requests to the correct ECS backend using Redis. Se usa un perfil: **ases**, con su propia infra y su archivo de entorno (`.env.ases`).

## Architecture

- **Router**: Validates header `apikey`, looks up Redis (`route:<instance_id>` or `backends` set), proxies HTTP to the right backend.
- **Redis**: Stores `route:<instance_id>` → backend URL and set `backends` (each backend registers on startup).
- **Backends**: Whatsmiau API; shared PostgreSQL (RDS) for all tasks; `WEBHOOK_URL` for all events; on session loss, instance and route are removed and `session.lost` is sent to the webhook.

## Perfil: ases

El perfil tiene su cuenta/rol AWS y su stack CloudFormation:

| Perfil | Archivo env | Uso |
|--------|-------------|-----|
| ases   | `.env.ases` | Infra y deploy del entorno ases |

Copia el ejemplo y rellena `API_KEY`, `WEBHOOK_URL`, y opcionalmente `ECR_REGISTRY` o `AWS_ACCOUNT_ID`:

```sh
cp .env.ases.example .env.ases
# Edita .env.ases (API_KEY, WEBHOOK_URL, AWS_PROFILE=ases, etc.)
```

## Environment variables (archivo .env.ases)

### Router (ECS task)

| Variable | Description |
|----------|-------------|
| `PORT` | Listen port (default 8080) |
| `REDIS_URL` | Redis host:port (e.g. from CloudFormation output) |
| `API_KEY` | Value clients must send in the `apikey` header |

### Backend (ECS task)

| Variable | Description |
|----------|-------------|
| `PORT` | Listen port (default 8080) |
| `REDIS_URL` | Same Redis as router |
| `API_KEY` | Same key as router |
| `WEBHOOK_URL` | URL where all WhatsApp events are sent |
| `DIALECT_DB` | `postgres` |
| `DB_URL` | `postgres://<user>:<pass>@<db-endpoint>:5432/<db>?sslmode=disable` |

### Infra-only vars (scripts/aws-infra.<profile>.env)

| Variable | Description |
|----------|-------------|
| `DB_MIN_ACU` | Aurora Serverless v2 minimum ACU (e.g. `0.5`) |
| `DB_MAX_ACU` | Aurora Serverless v2 maximum ACU (e.g. `4`) |
| `DB_NAME` | Database name |
| `DB_USERNAME` | Master username |
| `DB_PASSWORD` | Master password (required) |

## Deploy steps (scripts + Makefile)

### 1. Create infrastructure (first time)

Crea ECR y el stack CloudFormation (VPC, Redis, ECS, ALB):

```sh
# Infra para ases (usa .env.ases)
make infra-create PROFILE=ases
```

### 2. Build, push y deploy (cada vez que cambies código)

Construye las imágenes una vez, luego hace push y fuerza el deploy ECS en **ases** (usando `.env.ases`):

```sh
make push-prod
# o: ./scripts/aws-push-prod.sh
```

Requiere que exista `.env.ases`.

Importante: antes de `infra-create`, crea `scripts/aws-infra.ases.env` a partir del ejemplo y define `DB_PASSWORD`.

### 3. Update infrastructure (template o parámetros)

Tras cambiar `cloudformation/template.yaml` o parámetros, actualiza el stack:

```sh
make infra-update PROFILE=ases
```

### 4. Delete stack

```sh
make infra-destroy PROFILE=ases
```

### 5. API endpoint y dominio (Route 53)

El dominio `asesadmin.com` está en AWS (Route 53). Para **ases** la API se expone en `whatsmiau.asesadmin.com`, con un registro que apunta al ALB.

Obtener el nombre del ALB:

```sh
# Valores para ases (dominio whatsmiau.asesadmin.com)
make domain-info PROFILE=ases
```

Ver [docs/dominio.md](../docs/dominio.md) para configurar el registro DNS.

Llamar a la API (por ALB o por dominio cuando esté configurado):

```sh
curl -H "apikey: YOUR_API_KEY" http://<APIEndpoint>/v1/instance
# o con dominio: curl -H "apikey: YOUR_API_KEY" http://whatsmiau.asesadmin.com/v1/instance
```

## Flow

1. Client sends a request to the Router (ALB) with header `apikey`.
2. Router checks the secret, then looks up Redis: for paths with an instance id (e.g. `/v1/instance/MYINSTANCE/...`) it uses `GET route:MYINSTANCE` to get the backend URL; for create/list it uses `SMEMBERS backends` and picks one backend.
3. Router proxies the request to that backend.
4. Backend handles the request; on session loss it deletes the instance and route from Redis and sends `session.lost` to `WEBHOOK_URL`.
