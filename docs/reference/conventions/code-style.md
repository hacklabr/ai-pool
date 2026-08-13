# Convenção — Estilo de código

> Criado em: 2026-08-13 · Última revisão: 2026-08-13
> Regra: doc desatualizado é corrigido ou marcado como obsoleto — nunca deixado apodrecendo em silêncio.

Fonte: CI (`.github/workflows/ci.yml`), Makefile e padrões observados no código.

## Go

- **gofmt** obrigatório (gate de CI: `gofmt -l .` deve ser vazio). `go vet ./...` limpo.
- Testes com **race detector**: `go test -race -cover ./...` (target `make test`).
- Comentários e docs em **PT-BR**; identificadores e código em EN. (Exceção residual
  conhecida: 2 comentários em chinês — dívida registrada, não replicar.)
- Erros: sentinelas em `internal/domain` comparáveis com `errors.Is`; repos
  convertem erros de infra na borda (`ErrNotFound → domain.Err*`); erros wrapados
  como `fmt.Errorf("Repo.Method: %w", err)`. Respostas HTTP ao cliente são
  deliberadamente genéricas (anti-enumeração).
- **SQL sempre com placeholders `$1..$n`** (pgx) — nunca concatenação.
- Segredos decifrados são zeroizados (`defer zeroBytes`) e jamais logados; logs
  passam pelo `SanitizingHandler` global — usar sempre o logger do `main`,
  nunca `slog.Default()`.
- `#nosec` só com **justificativa inline obrigatória** (gate gosec no CI).
- Functional options para configurar componentes (`WithX...`); clock injetável
  (`now func() time.Time`) em componentes testáveis.
- Não propagar marcadores de fase históricos ("F3", "F10", "spec §x") em código
  novo — documentar o estado atual.

## Segurança (gates de CI)

- `gosec -severity medium` e `govulncheck ./...` — ambos rodam em
  `make ci-local` e no CI; rodar antes de push.

## Frontend (templates)

- `html/template` com escape automático; JS de terceiros só via CDN **com SRI**;
  CSS próprio self-contained (ITCSS/BEM) em `web/static/css/`.

## Verificação antes de commit

`make ci-local` (vet + race tests + gofmt + govulncheck + gosec).
