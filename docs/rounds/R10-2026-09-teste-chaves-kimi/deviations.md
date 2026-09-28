# Deviations of round R10 — teste de chaves: request válido p/ Kimi + 403 concorrente sem auth_failed

## No deviations in this round.

Registro de ocorrências que NÃO são desvios (planned == implemented em todo o
escopo):

- `formatTransientLimitMessage` extraído como helper (em vez de `fmt.Sprintf`
  inline no `case !persist`): previsto como opção no comentário técnico da
  issue #33 ("integrado onde fizer sentido").
- Falha do `govulncheck` no `make ci-local` local: incompatibilidade ambiental
  pré-existente (binário go1.26 × toolchain go1.27), reproduzida em árvore
  limpa — não é regressão da rodada; o job Security do CI (gosec + govulncheck)
  passou no PR #34.
