#!/usr/bin/env bash
# Muestra el ALB DNS name para configurar CNAME en IONOS (dominio externo).
# Uso: AWS_PROFILE=ases ./scripts/aws-domain-info.sh
#      AWS_PROFILE=foxy ./scripts/aws-domain-info.sh
set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/aws-config.env" 2>/dev/null || true
AWS_PROFILE="${AWS_PROFILE:-ases}"
export AWS_PROFILE
ENV_FILE="$REPO_ROOT/.env.$AWS_PROFILE"
[ ! -f "$ENV_FILE" ] && ENV_FILE="$REPO_ROOT/.env.production"
[ -f "$ENV_FILE" ] && set -a && source "$ENV_FILE" && set +a
STACK_NAME="${STACK_NAME:-whatsmiau}"
AWS_REGION="${AWS_REGION:-us-east-1}"

ALB_DNS=$(aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$AWS_REGION" \
  --query 'Stacks[0].Outputs[?OutputKey==`ALBDNSName`].OutputValue' --output text 2>/dev/null || true)
if [ -z "$ALB_DNS" ]; then
  echo "No se pudo obtener ALBDNSName (¿stack $STACK_NAME existe y tiene output ALBDNSName?)." >&2
  exit 1
fi
echo "Perfil: $AWS_PROFILE"
echo "Stack:  $STACK_NAME"
echo ""
echo "En IONOS crea un registro CNAME:"
echo "  Nombre (subdominio):  whatsmiau   (para whatsmiau.asesadmin.com o el dominio que uses)"
echo "  Destino / Valor:      $ALB_DNS"
echo ""
echo "URL de la API (cuando el DNS esté activo): http://whatsmiau.asesadmin.com  (o tu dominio)"
