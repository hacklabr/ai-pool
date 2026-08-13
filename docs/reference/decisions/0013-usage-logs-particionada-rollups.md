# ADR-0013 — usage_logs particionada mensalmente, com rollups horários

**Status:** aceito

**Contexto:** `usage_logs` tem o maior volume de escrita do sistema (1 linha por request). Tabela monolítica degradaria queries do dashboard e tornaria retenção (DROP de dados antigos) custosa. O dashboard precisa responder em ms sobre volumes crescentes.

**Decisão:** `usage_logs` é `PARTITION BY RANGE (created_at)` mensal, com partições pré-criadas (mês corrente + 3 à frente) e partição `DEFAULT` como armadilha de alerta; PK composta `(id, created_at)`; FKs são lógicas (restrição do PG em tabelas particionadas). Agregação horária em tabela física `usage_rollups` (não materialized view) via upsert idempotente (`ON CONFLICT DO UPDATE`), incluindo KPI `cached_tokens`. Pré-criação de partições por job periódico (não `pg_partman`).

**Consequências:** Positivas: retenção futura = DROP de partição (instantâneo); dashboard consulta rollups pequenos; índices propagam às partições. Negativas: **risco operacional** — INSERT fora de partição cai na DEFAULT (o job de criação de partições ainda **não está implementado** — pendência registrada no setup); PK composta e ausência de FK física são constraints permanentes do design.

**Evidências:** `migrations/202606252100_init_schema.up.sql`; `internal/usage/rollup_job.go`; `internal/adapters/postgres/usage_repo.go`.

**Origem:** DECISIONS.md A14 (consenso de especialistas 6-0).
