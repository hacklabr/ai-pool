// Package postgres implementa os adapters de persistência (PostgreSQL) para o
// AI Provider Pool, seguindo a arquitetura hexagonal definida em
// internal/domain (Ports) e na especificação §3.4.
//
// Componentes deste pacote:
//
//   - db.go:        connection pool (pgxpool v5) com defaults sensatos.
//   - migrate.go:   helper para aplicar migrations (golang-migrate + embed.FS).
//   - *_repo.go:    repositórios concretos que implementam as interfaces de
//     domínio (UserRepo, TokenRepo, ProviderRepo, KeyRepo,
//     UsageLogRepo). Métodos complexos para fases futuras
//     (F4-F9) ficam como stubs comentados.
//
// Decisões (spec §3.4):
//
//   - pgx/v5 + pgxpool (NÃO database/sql). Prepared statements via $1, $2...
//     NUNCA concatenação de strings em SQL.
//   - Pool: 60 conns default (calibrável via DB_MAX_CONNS). PgBouncer só no
//     roadmap quando réplicas crescerem.
//   - Migrations: golang-migrate com source/iofs (embed) + database/pgx/v5.
//     Aplicadas via Helm Job hook no deploy (DevOps §5.3).
//
// Segurança (spec §3.3.2, AGENTS.md):
//
//   - Tokens hasheados com HMAC-SHA256 + pepper (pepper NUNCA no DB).
//   - Chaves de provedor cifradas com AES-256-GCM (KEK NUNCA no DB).
//     A cifragem/descifragem acontece em F7; este pacote apenas persiste o
//     ciphertext opaco (BYTEA).
//   - Conexão TLS (sslmode=verify-full) obrigatória em produção.
package postgres
