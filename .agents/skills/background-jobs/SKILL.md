---
name: background-jobs
description: Use quando for criar ou alterar jobs periódicos em background (tickers, schedulers, collectors) — padrão único de lifecycle do projeto.
---

# Background jobs (ticker + ctx + RunOnce)

## Pré-requisitos

- Ver referências: `usage/collector.go`, `usage/rollup_job.go`,
  `routing/reactivation_scheduler.go`.

## Procedimento

1. Estrutura: goroutine com `time.Ticker` e `select ctx.Done()`; primeira execução
   imediata ou no primeiro tick, conforme a semântica do job.
2. Exporte `RunOnce(ctx)` para testes determinísticos — o loop chama `RunOnce`.
3. Injete o clock (`now func() time.Time`) via functional option — testes não
   podem depender de tempo real.
4. O lifecycle (start/cancel) é orquestrado manualmente no `cmd/aipp/main.go`:
   registre o cancel no shutdown gracioso (ADR-0002) — não há framework de jobs.
5. Falhas de uma iteração: logar (logger sanitizado do main) e continuar; nunca
   derrubar o processo por erro de job. Se o job grava no DB, considere
   idempotência (upsert / `ON CONFLICT`) e dead-letter quando aplicável.
6. Concorrência com o banco: use `FOR UPDATE SKIP LOCKED` quando múltiplas
   instâncias puderem varrer a mesma fila (ver `ReactivationScheduler`).

## Critérios de pronto

- [ ] Testes passando (`make test`)
- [ ] Tipos checados (`go build ./...`)
- [ ] Documentação atualizada se o contrato mudou
- [ ] Cancel registrado no graceful shutdown do main
