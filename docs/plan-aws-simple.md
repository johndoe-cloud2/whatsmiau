# Plan (simple y funcional): API única en AWS

## Objetivo

Una sola API pública. El **router** valida API secret y envía cada petición al **backend ECS** correcto según Redis. Cada backend: SQLite en el mismo contenedor, **WEBHOOK_URL** por env, mínimo 1 tarea y escalar cuando CPU o RAM > 80%. Si se pierde la sesión de WhatsApp: borrar instancia y ruta en Redis y avisar al webhook.

---

## Arquitectura (mínima)

- **Router**: valida header (API secret), busca en Redis `route:<instance_id>` → URL del backend; si no hay instance_id (ej. crear instancia), elige uno del set `backends` y hace proxy. Solo proxy HTTP.
- **Redis**: `route:<instance_id>` = URL del backend; `backends` = SET de URLs de backends (cada uno se registra al arranque).
- **Backend**: la app actual; SQLite en `/app/data`; al crear instancia escribe `route:<id>` y está en `backends`; al perder sesión borra instancia + ruta y envía evento al WEBHOOK_URL.

---

## 1. Router (mínimo)

- Validar header: nombre y valor en env (`API_SECRET_HEADER`, `API_SECRET`). Si falla → 401.
- De la ruta sacar `instance_id` cuando exista (ej. `/v1/instance/XXX/...` → XXX).
- Si hay `instance_id`: `GET route:<instance_id>` en Redis. Si no hay valor → 503.
- Si no hay `instance_id` (ej. POST crear): `SMEMBERS backends`, elegir uno (ej. el primero) y hacer proxy ahí.
- Proxy: reenviar método, path, body y headers relevantes al backend; devolver respuesta.
- Env: `REDIS_URL`, `API_SECRET`, `API_SECRET_HEADER`. Un solo binario en Go, sin DB.

---

## 2. Cambios en la API (backends)

- **Env**: `WEBHOOK_URL` (todos los eventos a esta URL), `BACKEND_PUBLIC_URL` (URL con la que este contenedor es alcanzable). Auth en backend opcional si solo recibe tráfico del router en red privada.
- **Webhook**: si `WEBHOOK_URL` está definido, usarlo siempre al emitir; si no, usar `instance.Webhook.Url` (comportamiento actual). Un solo cambio en `lib/whatsmiau/event_emitter.go`.
- **Redis**: clave `route:<instance_id>` = `BACKEND_PUBLIC_URL`. Set `backends`: al arranque `SADD backends <BACKEND_PUBLIC_URL>`. Tras crear instancia `SET route:<id> <BACKEND_PUBLIC_URL>`.
- **Al perder sesión** (`handleLoggedOut`): (1) borrar device y cliente como ahora, (2) `repo.Delete(ctx, id)`, (3) `DEL route:<id>`, (4) POST a WEBHOOK_URL con evento `SESSION_LOST` y `instance_id`.
- **DB**: SQLite por defecto: `DIALECT_DB=sqlite3`, `DB_URL=file:/app/data/data.db?_foreign_keys=on`. El Dockerfile ya tiene `/app/data`.

---

## 3. CloudFormation (mínimo)

- **VPC**: subnets pública y privada; NAT para que los backends lleguen a WhatsApp.
- **Redis**: ElastiCache Redis, 1 nodo, en subnets privadas.
- **ECS**: cluster; 1 task definition Router (imagen router, env Redis + secret); 1 task definition Backend (imagen whatsmiau, env Redis + WEBHOOK_URL + BACKEND_PUBLIC_URL + SQLite).
- **BACKEND_PUBLIC_URL**: script de entrypoint que lea la IP del task por metadata ECS, exporte `BACKEND_PUBLIC_URL=http://<ip>:8080` y ejecute el binario. Una sola imagen backend.
- **Servicios**: Router 1 tarea detrás del ALB. Backend desired count 1; scaling si CPU o memoria > 80% (máximo según necesidad).
- **ALB**: listener al Router (puerto 80; HTTPS opcional después). Sin Cloud Map: el router solo usa Redis (route + backends).

---

## 4. Env por componente

- **Router**: `REDIS_URL`, `API_SECRET`, `API_SECRET_HEADER`.
- **Backend**: `REDIS_URL`, `WEBHOOK_URL`, `BACKEND_PUBLIC_URL`, `DIALECT_DB=sqlite3`, `DB_URL=file:/app/data/data.db?_foreign_keys=on`, `PORT`.

---

## 5. Orden de implementación (todos)

- [ ] **backend-env-webhook**: Añadir env WEBHOOK_URL y BACKEND_PUBLIC_URL; usar WEBHOOK_URL al emitir en event_emitter.
- [ ] **backend-redis-route**: Registro en Redis (SADD backends al arranque, SET route:\<id\> al crear instancia).
- [ ] **backend-logged-out**: handleLoggedOut con repo.Delete + DEL route + evento SESSION_LOST al webhook.
- [ ] **backend-sqlite-defaults**: Defaults SQLite en env para despliegue ECS (DIALECT_DB, DB_URL).
- [ ] **router-service**: Servicio Go con auth por header, lookup Redis (route/backends), proxy HTTP.
- [ ] **backend-entrypoint**: Script que setea BACKEND_PUBLIC_URL desde metadata ECS y arranca la API.
- [ ] **cloudformation**: VPC, Redis, ECS router+backend, ALB, scaling CPU/RAM >80%, mín 1 tarea.
- [ ] **readme-deploy**: README o doc breve con env y pasos de despliegue.
