# AGENTS.md — AI Provider Pool

## 1. Contexto do projeto

Proxy/load-balancer de LLMs compatível com a API OpenAI (pool de chaves de
provedores com failover, afinidade de cache de contexto, painel admin
server-rendered). Stack: Go 1.26 (net/http puro no hot path) + PostgreSQL 16
(pgx/v5) + html/template + HTMX; deploy via Helm/K8s (imagem distroless).
A fonte de verdade do produto é o PRD vivo em `docs/reference/prd.md`.

## 2. Comandos verificáveis

| Ação | Comando |
|---|---|
| Build | `make build` |
| Testes | `make test` (go test -race -cover ./...) |
| Lint | `make vet` + gofmt (gate no `make ci-local`) |
| Typecheck | `go build ./...` (Go compilado; vet cobre análise estática) |
| CI completo local | `make ci-local` (vet + race tests + gofmt + govulncheck + gosec) |

Rode os comandos relevantes antes de declarar qualquer tarefa pronta.

## 3. Mapa da estrutura

- `cmd/aipp/` — entrypoint e composição (wiring) de todos os adapters; CLI
  (`--migrate-only`, `--bootstrap-admin-username`).
- `internal/domain/` — PURO: entidades, portas (interfaces), erros sentinelas.
  Nunca importa transporte nem infra.
- `internal/routing/` — motor de roteamento: LRU, afinidade, failover,
  auto-disable, reativação.
- `internal/usage/` — coletor assíncrono de uso + rollup job.
- `internal/adapters/postgres/` — pool pgx, migrations embed, repositórios.
- `internal/adapters/store/` — implementações in-memory das portas.
- `internal/adapters/crypto/` — envelope encryption AES-256-GCM (KEK externa).
- `internal/adapters/security/` — SanitizingHandler de logs.
- `internal/adapters/httpserver/` — proxy (net/http puro), failover wrapper, admin.
- `internal/config/` — config 12-factor via env.
- `migrations/` — fonte de verdade do schema (pares up/down, embed.FS).
- `web/` — templates + CSS (embed.FS).
- `deploy/helm/ai-pool/` — chart de produção.

Regra: nova dependência externa = porta mínima no pacote consumidor + adapter
em `postgres/` (ou `store/`) + wiring no `main.go`.

## 4. Regras invioláveis

- Nunca commitar sem rodar os testes (`make ci-local` antes de push).
- Nunca criar arquivos sem necessidade.
- Nunca editar migrations já aplicadas.
- Nunca adicionar dependências sem justificar.
- Nunca desativar checks de CI para fazer o build passar.
- Nunca logar segredos (chaves, tokens, KEK, hash de afinidade) — todo log
  passa pelo SanitizingHandler do `main`; nunca usar `slog.Default()`.
- SQL sempre com placeholders `$1..$n` — nunca concatenação.
- Nunca re-serializar o body do cliente no proxy (bytes originais ao upstream).

## 5. Convenções

As convenções vivem em `docs/reference/conventions/` (`code-style.md`,
`git-workflow.md`, `api-design.md`). Leia antes de escrever código — este
arquivo aponta, não duplica.

## 6. Workflow esperado

- Planeje antes de codar.
- Rode os testes antes de declarar pronto.
- Formato de commit e PR conforme `docs/reference/conventions/git-workflow.md`
  (Conventional Commits, descrição em PT-BR).
- Consulte `docs/reference/jornadas.md` antes de alterar fluxos de usuário.
- Este projeto roda o fluxo Maestra: demandas entram por triagem (issues com
  labels `Produto`/`Engenharia`/`Entrega`, board Projects #28).

## 7. Ponteiros

- `docs/reference/prd.md` → produto e requisitos (fonte de verdade)
- `docs/reference/jornadas.md` → fluxos de usuário
- `docs/reference/arquitetura/INDEX.md` → fonte de verdade da arquitetura
  (índice roteador — carregue cada doc só quando relevante)
- `docs/reference/decisions/` → ADRs (registros de decisão técnica)
- `.agents/skills/` → catálogo de procedimentos sob demanda
- `DECISIONS.md` (raiz) → registro histórico pré-fluxo (superseded pelos ADRs)

## Skills — procedimentos sob demanda

Regras sempre ativas ficam neste arquivo; procedimentos vivem em
`.agents/skills/`. Um procedimento só vira skill quando é repetível,
multi-etapa ou de alto custo de erro — e não-óbvio (se qualquer agente acerta
sem orientação, não precisa de skill).

**Evolução contínua:** quando uma decisão consolidada ou padrão recorrente
emergir no dia a dia (ex.: arquitetura de módulos definida, convenção de
widgets estabilizada), proponha uma skill usando
`.agents/skills/exemplo-skill/SKILL.md` como formato — nunca crie sem
aprovação explícita.

## ADRs são imutáveis

Decisão nova = ADR novo em `docs/reference/decisions/` (sequência de 4
dígitos a partir do máximo existente — atual: 0014), que referencia o
substituído. Nunca edite um ADR aceito; nunca renumere ADRs existentes.
Formato: `docs/reference/decisions/0000-template-adr.md`.
