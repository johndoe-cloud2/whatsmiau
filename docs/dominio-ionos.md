# Configurar dominio para la API

La API en AWS está detrás de un ALB. El dominio se configura según dónde esté registrado:

- **Ases**: el dominio **asesadmin.com** está en **AWS (Route 53)**. El registro `whatsmiau.asesadmin.com` se configura en Route 53 (ya configurado; para repetir o si cambia el ALB: `AWS_PROFILE=ases ./scripts/aws-route53-ases.sh`).
- **Foxy**: el dominio está en **IONOS**. Hay que crear un **CNAME** en IONOS apuntando al ALB (ver abajo).

## Resumen rápido

| Entorno | Dominio | Dónde se configura |
|---------|---------|---------------------|
| **ases** | `whatsmiau.asesadmin.com` | AWS Route 53 (script `aws-route53-ases.sh`) |
| **foxy** | p. ej. `whatsmiau.foxyadminbot.info` | IONOS → CNAME; valor del ALB con `AWS_PROFILE=foxy ./scripts/aws-domain-info.sh` |

---

## Ases (dominio en AWS Route 53)

El registro **whatsmiau.asesadmin.com** ya está creado en Route 53 apuntando al ALB. Si en el futuro cambias el ALB (p. ej. recreas el stack), vuelve a ejecutar:

```bash
AWS_PROFILE=ases ./scripts/aws-route53-ases.sh
```

---

## Foxy (dominio en IONOS)

### 1. Obtener el valor del ALB (destino del CNAME)

```bash
AWS_PROFILE=foxy ./scripts/aws-domain-info.sh
```

El script imprime el **destino** del CNAME. Copia ese valor.

### 2. Configurar en IONOS

1. IONOS → **Dominios** → tu dominio de foxy → **DNS**.
2. Añade **CNAME**: nombre `whatsmiau`, destino = valor del script, TTL por defecto.
3. Guarda; en unos minutos estará activo.

### 3. Comprobar

```bash
dig whatsmiau.<tu-dominio> CNAME +short
curl -H "apikey: TU_API_KEY" http://whatsmiau.<tu-dominio>/v1/instance
```

**Ases** (ya configurado en Route 53):

```bash
curl -H "apikey: TU_API_KEY" http://whatsmiau.asesadmin.com/v1/instance
```

---

**HTTPS (ases):** Para `whatsmiau.asesadmin.com` el ALB tiene listener 443 con certificado ACM. El ARN del certificado se configura en **infra**, no en el .env de la app: copia `scripts/aws-infra.ases.env.example` a `scripts/aws-infra.ases.env` y define `CERTIFICATE_ARN`. Lo usan solo `aws-create-stack.sh` y `aws-update-stack.sh`. En foxy, si no creas `aws-infra.foxy.env` con certificado, solo se expone HTTP.

---

## Si la URL de foxy no carga (p. ej. /health)

En foxy el ALB **solo tiene HTTP (puerto 80)** salvo que configures certificado. Usa **http://** y sigue estos pasos:

1. **Probar por HTTP (no HTTPS)**  
   Si usas `https://whatsmiau.foxyadminbot.info/health` puede hacer timeout porque no hay listener 443. Prueba:
   ```bash
   curl -sS -o /dev/null -w "%{http_code}" http://whatsmiau.foxyadminbot.info/health
   ```
   Deberías ver `200`. Si falla, sigue.

2. **Comprobar que el stack existe y dar el ALB**  
   ```bash
   AWS_PROFILE=foxy ./scripts/aws-domain-info.sh
   ```
   Si falla, el stack no existe o no tiene salida ALBDNSName (crear/actualizar con `make infra-create PROFILE=foxy` o `make infra-update PROFILE=foxy`).

3. **Comprobar DNS (CNAME en IONOS)**  
   El CNAME `whatsmiau` debe apuntar al valor que imprime el script anterior (el DNS del ALB, tipo `whatsmiau-xxxx.us-east-1.elb.amazonaws.com`). Comprueba:
   ```bash
   dig whatsmiau.foxyadminbot.info CNAME +short
   ```
   Debe devolver ese ALB. Si está vacío o distinto, en IONOS → Dominios → foxyadminbot.info → DNS, revisa el CNAME `whatsmiau` y pon como destino el ALB del paso 2.

4. **Probar directamente contra el ALB**  
   Con el ALB del paso 2:
   ```bash
   curl -sS http://<ALB_DNS_del_script>/health
   ```
   Si aquí responde `ok` pero por dominio no, el problema es solo DNS/CNAME.

5. **Si el ALB tampoco responde**  
   Revisa en AWS: ECS → cluster whatsmiau-whatsmiau → servicios `whatsmiau-router` y `whatsmiau-backend` (deben estar activos y con tareas running). El health check del ALB usa `GET /` al router; si el router no está sano, el ALB no enviará tráfico.
