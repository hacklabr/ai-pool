---
name: di-setter-nil-safe
description: Use quando for adicionar dependência opcional a um servidor/handler (proxy.Server, admin.App) — injeção via setter pós-construção com degradação graceful (503) quando ausente.
---

# DI via setters pós-construção (nil-safe → 503)

## Pré-requisitos

- Ver referências: `proxy/server.go` (`SetFailoverChain`, `SetRateLimiter`,
  `SetUsageCollector`) e `admin/admin.go` (`SetRateLimiter`, `SetEncryptor`, …).

## Procedimento

1. Dependências opcionais entram por **setter** (`SetX(dep)`), nunca por parâmetro
   novo no construtor — o construtor mantém assinatura mínima.
2. No handler, **cheque nil antes de usar** a dependência; se ausente, responda
   503 graceful (ver `admin.go:42-48`) — nunca panic.
3. A ordem de injeção no `cmd/aipp/main.go` importa: ao adicionar um setter novo,
   confira que o main chama-o antes de servir tráfego.
4. Ausência de DB/infra em dev segue deny-by-default (stubs `denyAll*` /
   `NoopUsageCollector`) — não "abra" o acesso para facilitar dev.

## Critérios de pronto

- [ ] Testes passando (`make test`)
- [ ] Tipos checados (`go build ./...`)
- [ ] Documentação atualizada se o contrato mudou
- [ ] Dependência nova tem cheque nil + 503 no handler e wiring no main
