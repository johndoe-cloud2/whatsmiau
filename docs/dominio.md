# Configurar dominio para la API

La API en AWS está detrás de un ALB. El dominio **asesadmin.com** está en **AWS (Route 53)**, así que el registro `whatsmiau.asesadmin.com` se configura en Route 53 (ya configurado; para repetir o si cambia el ALB: `AWS_PROFILE=ases ./scripts/aws-route53-ases.sh`).

## Resumen rápido

| Entorno | Dominio | Dónde se configura |
|---------|---------|---------------------|
| **ases** | `whatsmiau.asesadmin.com` | AWS Route 53 (script `aws-route53-ases.sh`) |

---

## Ases (dominio en AWS Route 53)

El registro **whatsmiau.asesadmin.com** ya está creado en Route 53 apuntando al ALB. Si en el futuro cambias el ALB (p. ej. recreas el stack), vuelve a ejecutar:

```bash
AWS_PROFILE=ases ./scripts/aws-route53-ases.sh
```

### Comprobar

```bash
dig whatsmiau.asesadmin.com +short
curl -H "apikey: TU_API_KEY" https://whatsmiau.asesadmin.com/v1/instance
```

---

**HTTPS:** Para `whatsmiau.asesadmin.com` el ALB tiene listener 443 con certificado ACM. El ARN del certificado se configura en **infra**, no en el .env de la app: copia `scripts/aws-infra.ases.env.example` a `scripts/aws-infra.ases.env` y define `CERTIFICATE_ARN`. Lo usan solo `aws-create-stack.sh` y `aws-update-stack.sh`. Si no defines certificado, solo se expone HTTP.

---

## Si la URL no carga (p. ej. /health)

1. **Probar el endpoint**
   ```bash
   curl -sS -o /dev/null -w "%{http_code}" https://whatsmiau.asesadmin.com/health
   ```
   Deberías ver `200`. Si falla, sigue. Si no hay certificado configurado, prueba con `http://`.

2. **Comprobar que el stack existe y dar el ALB**
   ```bash
   AWS_PROFILE=ases ./scripts/aws-domain-info.sh
   ```
   Si falla, el stack no existe o no tiene salida ALBDNSName (crear/actualizar con `make infra-create PROFILE=ases` o `make infra-update PROFILE=ases`).

3. **Comprobar DNS**
   El registro `whatsmiau.asesadmin.com` debe resolver al ALB del paso anterior (tipo `whatsmiau-xxxx.us-east-1.elb.amazonaws.com`). Comprueba:
   ```bash
   dig whatsmiau.asesadmin.com +short
   ```
   Si está vacío o distinto, vuelve a ejecutar `AWS_PROFILE=ases ./scripts/aws-route53-ases.sh`.

4. **Probar directamente contra el ALB**
   Con el ALB del paso 2:
   ```bash
   curl -sS http://<ALB_DNS_del_script>/health
   ```
   Si aquí responde `ok` pero por dominio no, el problema es solo DNS.

5. **Si el ALB tampoco responde**
   Revisa en AWS: ECS → cluster whatsmiau-whatsmiau → servicios `whatsmiau-router` y `whatsmiau-backend` (deben estar activos y con tareas running). El health check del ALB usa `GET /` al router; si el router no está sano, el ALB no enviará tráfico.
