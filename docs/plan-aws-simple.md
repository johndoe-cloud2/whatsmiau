# Plan (simple and functional): Single API on AWS

## Goal

One public API. The **router** validates API key (header `apikey`) and sends each request to the correct **ECS backend** via Redis. Each backend: SQLite in the same container, **WEBHOOK_URL** from env, minimum 1 task and scale when CPU or RAM > 80%. If a WhatsApp session is lost: delete instance and route in Redis and notify the webhook.

---

## Architecture (minimal)

- **Router**: validates header `apikey`, looks up Redis `route:<instance_id>` → backend URL; if there is no instance_id (e.g. create instance), picks one from the `backends` set and proxies. HTTP proxy only.
- **Redis**: `route:<instance_id>` = backend URL; `backends` = SET of backend URLs (each registers on startup).
- **Backend**: current app; SQLite in `/app/data`; on instance create writes `route:<id>` and is in `backends`; on session loss deletes instance + route and sends event to WEBHOOK_URL.

---

## 1. Router (minimal)

- Validate header `apikey` with value from env (`API_KEY`). On failure → 401.
- Extract `instance_id` from path when present (e.g. `/v1/instance/XXX/...` → XXX).
- If `instance_id` present: `GET route:<instance_id>` in Redis. If no value → 503.
- If no `instance_id` (e.g. POST create): `SMEMBERS backends`, pick one (e.g. first) and proxy there.
- Proxy: forward method, path, body and relevant headers to backend; return response.
- Env: `REDIS_URL`, `API_KEY`. Single Go binary, no DB.

---

## 2. API changes (backends)

- **Env**: `WEBHOOK_URL` (all events to this URL), `BACKEND_PUBLIC_URL` (URL at which this container is reachable). Auth on backend optional if it only receives traffic from the router on private network.
- **Webhook**: if `WEBHOOK_URL` is set, always use it when emitting; otherwise use `instance.Webhook.Url` (current behaviour). Single change in `lib/whatsmiau/event_emitter.go`.
- **Redis**: key `route:<instance_id>` = `BACKEND_PUBLIC_URL`. Set `backends`: on startup `SADD backends <BACKEND_PUBLIC_URL>`. After creating instance `SET route:<id> <BACKEND_PUBLIC_URL>`.
- **On session loss** (`handleLoggedOut`): (1) delete device and client as now, (2) `repo.Delete(ctx, id)`, (3) `DEL route:<id>`, (4) POST to WEBHOOK_URL with event `SESSION_LOST` and `instance_id`.
- **DB**: SQLite by default: `DIALECT_DB=sqlite3`, `DB_URL=file:/app/data/data.db?_foreign_keys=on`. Dockerfile already has `/app/data`.

---

## 3. CloudFormation (minimal)

- **VPC**: public and private subnets; NAT so backends can reach WhatsApp.
- **Redis**: ElastiCache Redis, 1 node, in private subnets.
- **ECS**: cluster; 1 task definition Router (router image, env Redis + secret); 1 task definition Backend (whatsmiau image, env Redis + WEBHOOK_URL + BACKEND_PUBLIC_URL + SQLite).
- **BACKEND_PUBLIC_URL**: entrypoint script reads task IP from ECS metadata, exports `BACKEND_PUBLIC_URL=http://<ip>:8080` and runs the binary. Single backend image.
- **Services**: Router 1 task behind ALB. Backend desired count 1; scaling when CPU or memory > 80% (max as needed).
- **ALB**: listener to Router (port 80; HTTPS optional later). No Cloud Map: router only uses Redis (route + backends).

---

## 4. Env per component

- **Router**: `REDIS_URL`, `API_KEY`.
- **Backend**: `REDIS_URL`, `API_KEY`, `WEBHOOK_URL`, `BACKEND_PUBLIC_URL`, `DIALECT_DB=sqlite3`, `DB_URL=file:/app/data/data.db?_foreign_keys=on`, `PORT`.

---

## 5. Implementation order (todos)

- [ ] **backend-env-webhook**: Add env WEBHOOK_URL and BACKEND_PUBLIC_URL; use WEBHOOK_URL when emitting in event_emitter.
- [ ] **backend-redis-route**: Register in Redis (SADD backends on startup, SET route:\<id\> on instance create).
- [ ] **backend-logged-out**: handleLoggedOut with repo.Delete + DEL route + SESSION_LOST event to webhook.
- [ ] **backend-sqlite-defaults**: SQLite defaults in env for ECS deployment (DIALECT_DB, DB_URL).
- [ ] **router-service**: Go service with header auth, Redis lookup (route/backends), HTTP proxy.
- [ ] **backend-entrypoint**: Script that sets BACKEND_PUBLIC_URL from ECS metadata and starts the API.
- [ ] **cloudformation**: VPC, Redis, ECS router+backend, ALB, scaling CPU/RAM >80%, min 1 task.
- [ ] **readme-deploy**: README or short doc with env and deploy steps.
