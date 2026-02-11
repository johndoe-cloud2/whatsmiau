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
