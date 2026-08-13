# ADR-0005 — Acesso a PostgreSQL via pgx v5 direto

**Status:** aceito

**Contexto:** O hot path (validação de token, resolução de chave, batch insert de uso) exige controle fino de queries (partial indexes, `FOR UPDATE SKIP LOCKED`, upserts, tabelas particionadas) e performance previsível. ORMs (GORM) ocultam esses recursos; sqlc adiciona toolchain de geração.

**Decisão:** Usar `jackc/pgx/v5` diretamente (pool via `pgxpool`), sem GORM nem sqlc. SQL explícito em repositórios por agregado (`TokenRepo`, `KeyRepo`, `UserRepo`, `UsageLogRepo` etc.).

**Consequências:** Positivas: acesso pleno a recursos PostgreSQL (partições, advisory locks, partial indexes); queries auditáveis; menos dependências. Negativas: SQL manual exige disciplina de sanitização (mitigado por prepared statements do pgx); sem geração de código, mudanças de schema exigem atualização manual de scans.

**Evidências:** `go.mod` (`jackc/pgx/v5`); `internal/adapters/postgres/db.go`, `token_repo.go`, `key_repo.go`, `usage_repo.go`.

**Origem:** DECISIONS.md A5 (consenso de especialistas 6-0).
