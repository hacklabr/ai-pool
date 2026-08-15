# Scope of round R06 — shell no container de produção via base distroless debug-nonroot

> Issue: #27 · Round R06 · 2026-08-15.

## Variant
minimal

## Requirements introduced
- (nenhuma RF nova — requisito operacional): capacidade de diagnóstico
  in-loco no container de produção (shell busybox) sem abrir mão de
  nonroot e da ausência de package manager.

## Requirements changed
- Base do stage de runtime no `Dockerfile`:
  `gcr.io/distroless/base-debian12:nonroot` →
  `gcr.io/distroless/base-debian12:debug-nonroot` (ADR-0016, substitui
  parcialmente ADR-0012). `USER nonroot` e `ENTRYPOINT` exec form mantidos.

## Requirements discontinued
- (nenhuma)

## Out of scope for this round
- Atualização das menções textuais a `:nonroot` em `docker-compose.yml`,
  `deploy/helm/` (values/README/migration-job) e
  `docs/reference/architecture.md` — comentários documentais, sem efeito
  de runtime; ficam para limpeza de documentação.
- Ephemeral debug containers (`kubectl debug`) como alternativa ao shell.
- Mudanças em código Go (nenhuma necessária).

## Métricas de sucesso
Incidentes de operação em produção passam a permitir diagnóstico direto
(`kubectl exec ... -- sh`), eliminando a dependência de inferência só por
logs; imagem continua sem package manager e rodando como UID 65532.

## Critérios de aceite (espelho da issue #27)
1. `FROM` final do Dockerfile é `gcr.io/distroless/base-debian12:debug-nonroot`.
2. `docker run --rm --entrypoint sh ai-provider-pool:debug-test -c 'echo shell-ok && id'`
   imprime `shell-ok` e `uid=65532(nonroot)` — shell presente, nonroot
   preservado.
3. `docker run --rm ai-provider-pool:debug-test --help` executa o binário
   (sem `exec format error`); exit != 0 do `--help` é aceitável.
4. `ENTRYPOINT` permanece em exec form, com comentário explicando que é
   boa prática (PID 1 + sinais), não dependência do distroless sem shell.
5. ADR-0016 criado (`docs/reference/decisions/0016-distroless-debug-nonroot.md`),
   Status: aceito, com contexto do incidente, alternativas, consequências
   honestas (superfície levemente maior) e referência ao ADR-0012 como
   parcialmente substituído. ADR-0012 permanece imutável.
6. `make vet` verde e `gofmt -l .` vazio (nenhum código Go alterado).
