# Configurar dominio en IONOS para la API

El dominio está gestionado en IONOS (no en AWS). Para que `whatsmiau.asesadmin.com` (ases) o el dominio que uses para foxy apunten a la API, hay que crear un **registro CNAME** en IONOS que apunte al DNS del ALB de AWS.

## 1. Obtener el valor del ALB (CNAME target)

Para **ases** (dominio ejemplo: `whatsmiau.asesadmin.com`):

```bash
AWS_PROFILE=ases ./scripts/aws-domain-info.sh
```

Para **foxy** (dominio ejemplo: `whatsmiau.foxyadmin.com` o el que uses):

```bash
AWS_PROFILE=foxy ./scripts/aws-domain-info.sh
```

O manualmente:

```bash
# Ases
aws cloudformation describe-stacks --stack-name whatsmiau --region us-east-1 --profile ases \
  --query 'Stacks[0].Outputs[?OutputKey==`ALBDNSName`].OutputValue' --output text

# Foxy
aws cloudformation describe-stacks --stack-name whatsmiau --region us-east-1 --profile foxy \
  --query 'Stacks[0].Outputs[?OutputKey==`ALBDNSName`].OutputValue' --output text
```

El resultado es un nombre tipo: `whatsmiau-XXXXX.us-east-1.elb.amazonaws.com`. Ese es el **valor de destino** del CNAME.

## 2. Configurar en IONOS

1. Entra en el panel de IONOS → Dominios → tu dominio (p. ej. `asesadmin.com`) → Gestión de DNS / DNS.
2. Añade un registro **CNAME**:
   - **Nombre / Host**: `whatsmiau` (para que sea `whatsmiau.asesadmin.com`). En algunos paneles se pone solo el subdominio, en otros el FQDN; si pide “nombre”, suele ser `whatsmiau`.
   - **Destino / Apunta a / Valor**: el valor de `ALBDNSName` que obtuviste antes (p. ej. `whatsmiau-XXXXX.us-east-1.elb.amazonaws.com`).
   - TTL: por defecto (ej. 3600).

3. Guarda y espera a que propague DNS (puede tardar unos minutos).

## 3. Comprobar

```bash
# Debe resolver al nombre del ALB
dig whatsmiau.asesadmin.com CNAME +short
```

Llamar a la API por dominio:

```bash
curl -H "apikey: TU_API_KEY" http://whatsmiau.asesadmin.com/v1/instance
```

## Resumen

| Entorno | Dominio (ejemplo) | CNAME destino |
|---------|-------------------|----------------|
| ases    | whatsmiau.asesadmin.com | Salida de `aws-domain-info.sh` con `AWS_PROFILE=ases` |
| foxy    | whatsmiau.foxyadmin.com (o el que uses) | Salida de `aws-domain-info.sh` con `AWS_PROFILE=foxy` |

El ALB escucha solo en HTTP (puerto 80). Si en el futuro quieres HTTPS, habría que añadir un certificado ACM y un listener 443 en el CloudFormation.
