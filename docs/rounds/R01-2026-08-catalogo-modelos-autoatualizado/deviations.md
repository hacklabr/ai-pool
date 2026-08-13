# Deviations of round R01 — catálogo de modelos autoatualizado

## Deviation 1 — Bump de golang.org/x/text v0.38.0 → v0.39.0
- **Planned:** nenhuma alteração de dependência (o briefing dizia "não deve precisar de nenhuma").
- **Implemented:** bump mínimo de dependência indireta `golang.org/x/text` v0.38.0 → v0.39.0.
- **Reason:** `govulncheck` falhava com GO-2026-5970 em código pré-existente (`usage/collector.go`, `postgres/migrate.go`) — o HEAD já estava vermelho antes desta round. O critério de aceite 7 exige `make ci-local` verde; sem o bump, o gate govulncheck reprova.
- **Decision registered at:** comentário de aceite da issue #18 (aceite por critério).
- **Reference document updated:** commit `fix(deps): bump golang.org/x/text para v0.39.0` em `go.mod`/`go.sum`.

## Deviation 2 — gofmt em internal/domain/entities.go
- **Planned:** não tocar `internal/domain/entities.go`.
- **Implemented:** apenas gofmt (alinhamento de comentários de struct).
- **Reason:** divergência pré-existente em HEAD (zero mudança de código lógico); o gate gofmt exige árvore limpa para `make ci-local` passar.
- **Decision registered at:** comentário de aceite da issue #18 (aceite por critério).
- **Reference document updated:** commit `feat(modelsync): adiciona sincronizador fail-safe do catálogo de modelos` em `internal/domain/entities.go` (apenas formatação).
