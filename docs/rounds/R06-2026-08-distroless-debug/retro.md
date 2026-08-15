# Retro of round R06 — shell no container via distroless debug-nonroot

## Round signals
- **Specification gaps:** 0.
- **New requirements discovered in Stage 3:** 0.
- **Late infeasibilities:** 0.
- **Documentation contradictions (`doc-bug`):** 1 (menor) — `AGENTS.md`
  aponta "atual: 0014" como máximo de ADR, mas o máximo real é 0015; a
  numeração correta (0016) foi confirmada listando o diretório. Menções
  textuais defasadas a `:nonroot` em compose/helm/architecture.md ficaram
  fora de escopo desta round (registradas no scope como out of scope).
- **Registered overrides:** 0.
- **Feedback:** 0.

## Process learnings
- Troca de variante de imagem é verificável de forma barata e definitiva
  no próprio build: dois `docker run` (shell + `--help`) provam
  busybox-presente, nonroot e execução do binário — melhor evidência que
  revisão de diff.
- O `ENTRYPOINT` exec form continuou válido por razão DIFERENTE da
  original (antes: exigência do distroless sem shell; agora: PID 1 +
  entrega de sinais do graceful shutdown). Vale sempre reescrever o
  comentário de "porquê" quando a justificativa muda — comentário com
  justificativa defasada é doc-bug latente.
- O incidente que motivou a round (500 no sync sem diagnóstico) é o
  argumento central do ADR-0016: contexto de ADR deve registrar a força
  real (MTTR), não só a escolha técnica.
