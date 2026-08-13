---
name: convencao-migrations
description: Use quando for criar ou revisar migrations SQL deste projeto (mudanças de schema em migrations/).
---

# Convenção de migrations

## Pré-requisitos

- Ler `migrations/README.md` e ADR-0006 (`docs/reference/decisions/0006-golang-migrate-helm-job-hook.md`).

## Procedimento

1. Crie o esqueleto com `make migrate-new name=slug_descritivo` — nomes `{YYYYMMDDHHMM}_{slug}.up.sql` / `.down.sql`.
2. Escreva SEMPRE o par up/down reversível. Nunca edite migration já aplicada.
3. Toda tabela/coluna nova leva `COMMENT ON` explicando o propósito.
4. Toda tabela multitenant tem `organization_id BIGINT NOT NULL DEFAULT 1` (ADR-0007); índices compostos começam por `organization_id`.
5. Partial indexes de hot path levam comentário explicando o caminho quente que atendem.
6. Nunca seed de credenciais em SQL (ver comentário em `init_schema.up.sql`).
7. Tabelas particionadas e `audit_log` usam FK lógica (sem CONSTRAINT física) — restrição do PG e decisão de append-only (ADR-0014).
8. Backward-compatible durante rollouts: pods antigos e novos convivem (ADR-0006).
9. Verifique se a mudança exige partição futura ou impacta `usage_logs` (particionada mensal — ver ADR-0013).

## Critérios de pronto

- [ ] Testes passando (`make test`)
- [ ] Tipos checados (`go build ./...`)
- [ ] Documentação atualizada se o contrato mudou
- [ ] Migration aplica e reverte limpa (`make migrate-up` / `make migrate-down` em banco local)
