# Causas raíz de 502 y 504 en el Router

Este documento explica **por qué** ocurrían los errores 502 Bad Gateway y 504 Gateway Time-out al llamar al API (p. ej. `POST /v1/instance`) y qué cambios se hicieron en la raíz.

## Flujo de la petición

```
Cliente → (optional: IONOS/proxy) → ALB (AWS) → Router (ECS) → Backend(s) (ECS)
                                         ↓
                                      Redis (backends, route:*)
```

- El **Router** valida `apikey`, mira en Redis qué backend usar (set `backends` para crear/listar, o `route:<instance_id>` para una instancia concreta) y hace **proxy HTTP** al backend.
- Cada **Backend** al arrancar hace `SADD backends <su URL>` y al crear una instancia hace `SET route:<id> <su URL>`.

---

## 1. 502 Bad Gateway y "context canceled"

### Qué se veía

- Respuesta **502** al hacer `POST /v1/instance`.
- En logs del router: `proxy to backend failed ... error: context canceled` para **todos** los backends (incluso el que sí estaba vivo).

### Causa raíz

- El router reutilizaba **el mismo contexto de la petición HTTP entrante** para **cada** intento de proxy hacia un backend.
- Ese contexto se cancela cuando:
  - el **cliente** cierra la conexión o hace timeout, o
  - el **ALB** corta por idle timeout (p. ej. 60 s).
- En cuanto el contexto se cancelaba (p. ej. en el primer intento o por timeout del ALB), **todos** los intentos siguientes usaban ya un contexto cancelado, así que el proxy fallaba de inmediato con **"context canceled"** y devolvía 502. No era que los backends no respondieran; era que el router dejaba de esperar en cuanto el contexto estaba cancelado.

### Solución (en código)

- Para las peticiones **sin** `instance_id` (como `POST /v1/instance`), en cada intento de backend se usa una **petición clonada con un contexto nuevo** (independiente del cliente/ALB), con un timeout propio (p. ej. 90 s por intento):
  - `reqTry := r.Clone(tryCtx)` con `tryCtx = context.WithTimeout(context.Background(), perBackendTimeout)`.
- Así, un cierre o timeout del cliente/ALB no cancela los intentos hacia los backends; cada intento tiene su propia ventana de tiempo.

---

## 2. Demasiados backends “muertos” en Redis

### Qué se veía

- En Redis el set `backends` tenía **muchas** URLs (varias IPs de ECS).
- Casi todas correspondían a **tareas viejas** que ya no existían; solo una (o ninguna) era la tarea actual.

### Causa raíz

- Cuando una tarea ECS se para (deploy, scale-in, crash), el proceso recibe SIGTERM pero **no** quitaba su URL del set `backends` en Redis.
- Redis nunca limpiaba esas URLs, así que el router seguía intentando con IPs muertas, gastando intentos y tiempo hasta dar 502 (o 504 si se pasaba del timeout del ALB).

### Solución (en código)

- **Backend**: al recibir SIGTERM/SIGINT, llama a `UnregisterBackend` (en Redis hace `SREM backends <url>`).
- **Router**: cuando un intento de proxy a un backend devuelve 502 (backend inalcanzable), hace `SREM backends <url>` para no volver a usar esa URL. Así se van eliminando backends muertos con el uso.

---

## 3. 504 Gateway Time-out

### Qué se veía

- Respuesta **504** (HTML típico de “Gateway Time-out”) al hacer `POST /v1/instance`, aunque el backend sí podía estar creando la instancia.

### Causa raíz

- El **504 lo devuelve el ALB** (o un proxy delante), no el router. Ocurre cuando el ALB **no recibe** una respuesta del router dentro de su **idle timeout** (por defecto **60 s**).
- Dos motivos por los que el router podía tardar más de 60 s:
  1. **Varios backends**: intentar muchos backends (p. ej. 9), cada uno con un timeout largo (p. ej. 25 s), hace que el tiempo total supere 60 s → el ALB cierra y devuelve 504.
  2. **Una operación lenta**: crear una instancia de WhatsApp (inicializar cliente, generar QR, etc.) puede tardar **60–90 s**. Si el timeout por backend o el del ALB son menores, el ALB corta la conexión antes de que el router pueda devolver la respuesta del backend → 504.

Es decir, la raíz es: **los timeouts en la cadena (ALB y, si aplica, cliente) eran más cortos que el tiempo que tarda la operación real**.

### Solución (en código e infra)

- **ALB**: en CloudFormation se sube el idle timeout del ALB (p. ej. `IdleTimeoutSeconds: 180`) para que operaciones lentas como crear instancia no sean cortadas por el ALB.
- **Router**: se limita el número de intentos de backend por petición (p. ej. máximo 2) y se usa un timeout por intento (p. ej. 90 s) coherente con esa operación, para no alargar innecesariamente la petición y seguir por debajo del timeout del ALB.

---

## Resumen de causas raíz

| Síntoma | Causa raíz | Cambio principal |
|--------|------------|-------------------|
| 502 + "context canceled" en todos los backends | Uso del contexto de la petición del cliente para todos los intentos de proxy; al cancelarse (cliente/ALB), todos fallan | Usar `r.Clone(tryCtx)` con contexto nuevo por intento |
| 502 por backends inalcanzables | URLs de tareas ECS muertas siguen en Redis (`backends`) | Backend: UnregisterBackend en SIGTERM; Router: SREM al recibir 502 |
| 504 Gateway Time-out | ALB (y/o cliente) cierra por idle timeout antes de que el router responda (muchos intentos u operación lenta) | ALB: IdleTimeoutSeconds 180; Router: timeout e intentos acotados (90 s, max 2 backends) |

---

## Cómo comprobar que está resuelto

- **Router**: en logs no deben aparecer “context canceled” en cascada para todos los backends; si un backend falla, el siguiente intento debe usar contexto nuevo.
- **Redis**: `SMEMBERS backends` debería tener pocas URLs (las de tareas actuales); las muertas se van quitando con SREM al 502 y con UnregisterBackend al apagar.
- **504**: tras subir el idle timeout del ALB y desplegar el router, las llamadas a `POST /v1/instance` que tarden ~60–90 s deberían devolver 200 (o el código que devuelva el backend), no 504.

---

## Si en local funciona pero desplegado no

1. **Probar el API desplegado y ver el código exacto**
   ```bash
   make api-test PROFILE=ases
   ```
   Ejecuta `scripts/aws-api-test.sh`: hace GET /health, GET /v1/instance y POST /v1/instance contra la URL de prod (p. ej. https://whatsmiau.asesadmin.com) con el `apikey` de `.env.ases`. Así ves si recibes **401** (apikey mal), **503** (no backends), **502** (backend inalcanzable) o **504** (timeout).

2. **Revisar logs del router justo después**
   ```bash
   make logs-fetch HOURS=1
   ```
   Busca en los logs del **router**:
   - `"no backends available"` y `backends_count: 0` → ningún backend se registró en Redis (revisar que el backend arranque bien y que en logs del backend salga `registered backend in Redis`).
   - `"proxying to backends"` y `backends_count: 1` (o más) → el router sí ve backends; si aun así recibes 502, el backend no es alcanzable desde el router (red/security groups).
   - `"backend responded"` y `code: 200` → el backend respondió bien; si el cliente sigue viendo 502/504, puede ser timeout del ALB o del cliente.

3. **Revisar logs del backend**
   En los mismos logs, en **backend**: que aparezca `BACKEND_PUBLIC_URL=http://10.x.x.x:8080` y `registered backend in Redis`. Si no, el entrypoint no está obteniendo la IP (revisar ECS metadata / entrypoint).

4. **Diferencias local vs desplegado**
   - Local: backend y router suelen estar en la misma red (Docker) y el backend se registra con una URL a la que el router puede conectar.
   - Desplegado: el backend se registra con su IP privada de ECS (10.0.x.x). El router tiene que poder conectar a esa IP (misma VPC; security group del backend debe permitir 8080 desde el security group del router). Si `backends_count > 0` pero siempre 502, suele ser red o security groups.
