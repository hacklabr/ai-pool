# Convenção — Workflow git

> Criado em: 2026-08-13 · Última revisão: 2026-08-13
> Regra: doc desatualizado é corrigido ou marcado como obsoleto — nunca deixado apodrecendo em silêncio.

Fonte: histórico de commits (`git log`) e CI.

## Branching

- `main` é a branch protegida; trabalho em branches de feature/fix com merge via PR.
- Implementações pelo fluxo Maestra usam **worktree** por tarefa (regra do fluxo).

## Commits

Formato **Conventional Commits** com descrição em PT-BR (observado no histórico):

```
<tipo>(<escopo>): <descrição curta em PT-BR>
```

- Tipos observados: `feat`, `fix`, `chore`, `docs`, `refactor`, `test`.
- Escopo = área afetada, ex.: `feat(models)`, `fix(security)`, `fix(ui)`,
  `chore(config)`, `feat(invitations)`.
- Descrição curta no imperativo, sem ponto final.

## CI/CD

- Push em `main` → build + push de imagem `ghcr.io/hacklabr/ai-pool:<sha>` + deploy-dev.
- Tag `v*` → release semver + latest + deploy-prod (Helm).
- Gates de CI (todos obrigatórios, nunca desativar para passar): go vet,
  gofmt, test -race, gosec, govulncheck, trivy (HIGH/CRITICAL), helm lint/template.
- Rodar `make ci-local` antes de push para evitar CI vermelho.

## Migrations

- Nunca editar migration já aplicada; nova mudança = novo par `{timestamp}_{slug}.{up|down}.sql`
  (`make migrate-new name=slug_descritivo`), sempre reversível.
