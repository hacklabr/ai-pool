#!/usr/bin/env bash
# scripts/gen-kek.sh — Gera uma KEK (Key Encryption Key) dev local para o
# AES-256-GCM envelope encryption das chaves de provedores no PostgreSQL.
#
# Gera 32 bytes aleatórios (256 bits) base64 e salva em kek-dev.key (no repo
# root, gitignored). O arquivo deve ser montado no container em /run/secrets/kek
# (ver docker-compose.yml, volume comentado).
#
# Uso:
#   ./scripts/gen-kek.sh                   # gera ./kek-dev.key
#   ./scripts/gen-kek.sh /path/to/output   # gera no caminho dado
#
# Após gerar:
#   1. No .env.docker: KEK_PATH=/run/secrets/kek
#   2. No docker-compose.yml, descomente o volume:
#        - ./kek-dev.key:/run/secrets/kek:ro
#   3. docker compose --env-file .env.docker up --build
#
# AVISO: a KEK NUNCA deve ser commitada. kek-dev.key já está no .gitignore.
# Em produção, a KEK vem de External Secrets Operator / Vault / KMS (spec §3.3.2).
#
# Entropia: usamos /dev/urandom (suficiente em Linux; CSPRNG do kernel).
# Em macOS/BSD, /dev/urandom também funciona.
set -euo pipefail

OUTPUT="${1:-kek-dev.key}"

# Sanity check: openssl disponível? Preferimos openssl por portabilidade.
if ! command -v openssl >/dev/null 2>&1; then
    echo "ERRO: openssl não encontrado no PATH." >&2
    echo "Instale openssl ou use:" >&2
    echo "  head -c 32 /dev/urandom | base64 > \"$OUTPUT\"" >&2
    exit 1
fi

# Gera 32 bytes aleatórios e codifica em base64 (44 chars sem newline).
# O app decodifica base64 ao ler; aceita também raw bytes.
KEY_B64="$(openssl rand -base64 32)"

# Escreve sem newline trailing (o app lê via os.ReadFile, ok com ou sem).
printf '%s' "$KEY_B64" > "$OUTPUT"
chmod 0600 "$OUTPUT"

# Caminho absoluto para a mensagem (clareza).
ABS="$(cd "$(dirname "$OUTPUT")" && pwd)/$(basename "$OUTPUT")"

cat <<EOF
OK: KEK dev gerada em:
  $ABS  (chmod 0600)

Próximos passos:

  1. No .env.docker, defina:
       KEK_PATH=/run/secrets/kek

  2. No docker-compose.yml (serviço app, seção volumes), descomente:
       - ./kek-dev.key:/run/secrets/kek:ro

  3. Suba o ambiente:
       docker compose --env-file .env.docker up --build

  4. Verifique no log do app:
       "AES-256-GCM envelope encryption enabled" (kek_id=v1)

AVISO: este arquivo ($ABS) é um SECRET local. NÃO comitar.
       (já está em .gitignore: kek-dev.key)

Para PRODUÇÃO, a KEK vem de External Secrets Operator/Vault/KMS,
NUNCA de um arquivo local.
EOF
