---
name: portas-e-adapters
description: Use quando um pacote precisar de uma nova dependência externa (DB, HTTP client, serviço) — define a porta mínima local, o adapter e o wiring, conforme a arquitetura hexagonal (ADR-0003).
---

# Portas mínimas locais + adapters (arquitetura hexagonal)

## Pré-requisitos

- Ler ADR-0003 e o mapa de módulos em `docs/reference/architecture.md` §2.

## Procedimento

1. A dependência externa vira uma **interface mínima definida no pacote consumidor**
   (ex.: `routing.EngineDeps`, `routing.KeyRepoPort`, `usage.UsageRepoPort`) — o
   consumidor declara só os métodos que usa, permitindo mock em teste.
2. A implementação concreta vive em `internal/adapters/postgres/` (ou `store/`,
   `crypto/`…), tipicamente em `routing_adapters.go` ou um arquivo dedicado.
3. Adicione o compile-time assertion no adapter:
   `var _ <pacote>.<Porta> = (*<Adapter>)(nil)`.
4. Faça o wiring no `cmd/aipp/main.go` — a composição é exclusiva do main.
5. `internal/domain/` permanece puro: nunca importe pgx, net/http ou outra infra nele.
6. Tipos de persistência (ex.: `UsageLogRecord`) NÃO sobem para o domain — ficam
   no pacote do repo (ver `usage_repo.go`).

## Critérios de pronto

- [ ] Testes passando (`make test`)
- [ ] Tipos checados (`go build ./...`)
- [ ] Documentação atualizada se o contrato mudou
- [ ] Compile-time assertion presente e wiring feito no main
