# Diretório de migrations (golang-migrate)

As migrations reais (schema PostgreSQL com ~11 entidades) serão entregues na **F2**
(tarefa T3 da especificação). Este diretório existe desde a F1 para que o `go:embed`
do binário possa referenciá-lo e o toolchain de migrations já tenha um alvo.

## Convenção de nomes

`{YYYYMMDDHHMM}_{slug_descritivo}.{up|down}.sql`

Exemplo:
- `202606252100_init_schema.up.sql`
- `202606252100_init_schema.down.sql`

## Ferramenta

[golang-migrate/migrate](https://github.com/golang-migrate/migrate) v4.

## Aplicação

Em produção: Helm pre-install/pre-upgrade Hook Job rodando `aipp migrate up`.
Em dev: init-container ou comando `make migrate-up`.

## Disciplina (spec §3.4.5 / DBA §8.2)

- Toda migration reversível (down funcional).
- Sem DDL destrutivo em up: `DROP COLUMN` vira rename + nullable + remover depois.
- `CREATE INDEX CONCURRENTLY` para indexes em tabelas grandes.
- `ADD COLUMN ... NOT NULL DEFAULT` em 2 passos quando há muitos registros.
