# Deploying Whatsmiau on AWS (single API with Router + ECS)

This describes the minimal AWS setup: one public API (Router) that validates the API key (header `apikey`) and routes requests to the correct ECS backend using Redis. Hay dos perfiles: **ases** y **foxy**, cada uno con su propia infra y su archivo de entorno (`.env.ases`, `.env.foxy`).

## Architecture

- **Router**: Validates header `apikey`, looks up Redis (`route:<instance_id>` or `backends` set), proxies HTTP to the right backend.
- **Redis**: Stores `route:<instance_id>` → backend URL and set `backends` (each backend registers on startup).
- **Backends**: Whatsmiau API; SQLite in `/app/data`; `WEBHOOK_URL` for all events; on session loss, instance and route are removed and `session.lost` is sent to the webhook.

## Perfiles: ases y foxy

Cada perfil tiene su propia cuenta/rol AWS y su stack CloudFormation (mismo template). Se usan archivos de entorno separados:

| Perfil | Archivo env | Uso |
|--------|-------------|-----|
| ases   | `.env.ases` | Infra y deploy del entorno ases |
| foxy   | `.env.foxy` | Infra y deploy del entorno foxy |

Copia los ejemplos y rellena `API_KEY`, `WEBHOOK_URL`, y opcionalmente `ECR_REGISTRY` o `AWS_ACCOUNT_ID`:

```sh
cp .env.ases.example .env.ases
cp .env.foxy.example .env.foxy
# Edita .env.ases y .env.foxy (API_KEY, WEBHOOK_URL, AWS_PROFILE=ases/foxy, etc.)
```

## Environment variables (por archivo .env.ases / .env.foxy)

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
| `DIALECT_DB` | `sqlite3` |
| `DB_URL` | `file:/app/data/data.db?_foreign_keys=on` |

## Deploy steps (scripts + Makefile)

### 1. Create infrastructure (first time) por perfil

Crea ECR y el stack CloudFormation (VPC, Redis, ECS, ALB) para el perfil indicado:

```sh
# Infra para ases (usa .env.ases)
make infra-create PROFILE=ases

# Infra para foxy (usa .env.foxy)
make infra-create PROFILE=foxy
```

### 2. Build, push y deploy en ambos perfiles (cada vez que cambies código)

Construye las imágenes una vez, luego hace push y fuerza el deploy ECS en **ases** y **foxy** (usando `.env.ases` y `.env.foxy`):

```sh
make push-prod
# o: ./scripts/aws-push-prod.sh
```

Requiere que existan `.env.ases` y `.env.foxy`. Para desplegar solo en un perfil puedes usar `AWS_PUSH_PROFILES=ases ./scripts/aws-push-prod.sh` (o solo `foxy`).

### 3. Update infrastructure (template o parámetros)

Tras cambiar `cloudformation/template.yaml` o parámetros, actualiza el stack del perfil que toque:

```sh
make infra-update PROFILE=ases
make infra-update PROFILE=foxy
```

### 4. Delete stack

```sh
make infra-destroy PROFILE=ases
make infra-destroy PROFILE=foxy
```

### 5. API endpoint y dominio (IONOS)

El dominio está en IONOS (no en AWS). Para **ases** la API se expone en `whatsmiau.asesadmin.com`; para **foxy** usas el dominio que tengas en IONOS. En ambos casos configuras un CNAME en IONOS apuntando al ALB.

Obtener el nombre del ALB (destino del CNAME):

```sh
# Valores para ases (dominio whatsmiau.asesadmin.com)
make domain-info PROFILE=ases

# Valores para foxy
make domain-info PROFILE=foxy
```

Ver [docs/dominio-ionos.md](../docs/dominio-ionos.md) para configurar el CNAME en IONOS.

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
