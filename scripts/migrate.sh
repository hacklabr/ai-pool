#!/usr/bin/env bash
# scripts/migrate.sh — wrapper para o CLI golang-migrate.
#
# Uso:
#   ./scripts/migrate.sh up          # aplica todas as migrations pendentes
#   ./scripts/migrate.sh down        # reverte a última migration
#   ./scripts/migrate.sh version     # mostra a versão atual do schema
#   ./scripts/migrate.sh force <V>   # força a versão (use com cautela)
#   ./scripts/migrate.sh new <nome>  # cria esqueleto de nova migration
#
# Variáveis de ambiente:
#   DATABASE_URL (ou DB_DSN) — string de conexão PostgreSQL.
#     Ex.: postgres://aipp:aipp@localhost:5432/aipp?sslmode=disable
#
# Requer o binário `migrate` no PATH (github.com/golang-migrate/migrate/v4).
#   Instalar: go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
#
# Em produção: as migrations rodam via Helm Job hook (DevOps §5.3), NÃO por
# este script. Este script é para desenvolvimento/teste local.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
MIGRATIONS_DIR="$REPO_ROOT/migrations"

# DSN: DATABASE_URL tem precedência; cai para DB_DSN.
DSN="${DATABASE_URL:-${DB_DSN:-}}"
if [[ -z "$DSN" ]]; then
  echo "ERRO: defina DATABASE_URL ou DB_DSN." >&2
  echo "Ex.: export DATABASE_URL='postgres://aipp:aipp@localhost:5432/aipp?sslmode=disable'" >&2
  exit 1
fi

if ! command -v migrate >/dev/null 2>&1; then
  echo "ERRO: binário 'migrate' não encontrado no PATH." >&2
  echo "Instale: go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest" >&2
  exit 1
fi

CMD="${1:-}"
case "$CMD" in
  up)
    migrate -path "$MIGRATIONS_DIR" -database "$DSN" up
    ;;
  down)
    # down reverte APENAS a última migration (mais seguro que down sem count).
    migrate -path "$MIGRATIONS_DIR" -database "$DSN" down 1
    ;;
  version)
    migrate -path "$MIGRATIONS_DIR" -database "$DSN" version
    ;;
  force)
    [[ $# -ge 2 ]] || { echo "Uso: $0 force <versao>"; exit 1; }
    migrate -path "$MIGRATIONS_DIR" -database "$DSN" force "$2"
    ;;
  new)
    [[ $# -ge 2 ]] || { echo "Uso: $0 new <nome_descritivo>"; exit 1; }
    # Convenção do projeto: {YYYYMMDDHHMM}_{slug}.{up|down}.sql (ver migrations/README.md).
    TS="$(date -u +%Y%m%d%H%M)"
    SLUG="$2"
    UP="$MIGRATIONS_DIR/${TS}_${SLUG}.up.sql"
    DOWN="$MIGRATIONS_DIR/${TS}_${SLUG}.down.sql"
    cat > "$UP"   <<EOF
-- Migration ${TS}_${SLUG} (UP).
-- Descreva o objetivo aqui.
EOF
    cat > "$DOWN" <<EOF
-- Reverso de ${TS}_${SLUG} (DOWN).
-- Ordem inversa de criação para respeitar FKs.
EOF
    echo "Criado:"
    echo "  $UP"
    echo "  $DOWN"
    ;;
  *)
    echo "Uso: $0 {up|down|version|force <V>|new <nome>}" >&2
    exit 1
    ;;
esac
