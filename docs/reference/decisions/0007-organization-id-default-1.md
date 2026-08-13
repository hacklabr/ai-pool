# ADR-0007 — organization_id NOT NULL DEFAULT 1 em todas as tabelas

**Status:** aceito

**Contexto:** Multi-tenancy real está no ROADMAP, mas retrofit de `organization_id` em tabelas populadas — especialmente `usage_logs` particionada — seria uma migração cara e arriscada. O MVP opera com um único tenant (org id=1).

**Decisão:** Toda tabela multitenant tem `organization_id BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id)`, com seed da organização default (id=1) na migration inicial. Índices compostos começam por `organization_id`; código de domínio carrega `OrgID` em todas as entidades e queries.

**Consequências:** Positivas: multi-tenancy futuro é habilitação de dados, não migração de schema; queries já filtram por org (isolamento desde o MVP); custo zero no MVP (DEFAULT 1). Negativas: coluna "morta" aparente no MVP; todo repo precisa do parâmetro orgID mesmo com um único tenant (verbosidade aceita conscientemente).

**Evidências:** `migrations/202606252100_init_schema.up.sql`; `internal/domain/entities.go`; `internal/config/config.go` (`DefaultOrgID`).

**Origem:** DECISIONS.md A7 (consenso de especialistas 6-0).
