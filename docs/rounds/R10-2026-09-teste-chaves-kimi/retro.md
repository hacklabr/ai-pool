# Retro of round R10 — teste de chaves: request válido p/ Kimi + 403 concorrente sem auth_failed

## Round signals
- **Specification gaps:** 1 — RF-03 especificava "401/403 marca auth_failed"
  por status code, sem conhecer a semântica dupla do 403 no Kimi Code (auth ×
  limite concorrente). Descoberta em teste operacional, não em especificação.
  (O body de teste com `temperature` fixa nunca foi especificado — detalhe de
  implementação que invalidava um provedor inteiro.)
- **New requirements discovered:** 3 candidatas, fora do escopo — registradas
  para triagem futura: (a) Trivy/CVEs na imagem base falhando na pipeline
  desde após v0.11.0 (crônico, pré-existente); (b) Deploy PROD (Helm) falhando
  desde a v0.11.0; (c) 429 `engine_overloaded_error` tratado como quota da
  chave quando a doc do Kimi o classifica como capacidade do servidor
  (transitório).
- **Late infeasibilities:** 0.
- **Documentation contradictions (`doc-bug`):** 0 — PRD RF-03 atualizado na
  mesma rodada (o bug 2 alterava especificação).
- **Registered overrides:** 0.
- **Feedback:** humano validou a interpretação do 403 contra a doc oficial do
  Kimi ANTES do merge — checkpoint de produto em fluxo técnico funcionou.

## Process learnings
- Teste operacional ao vivo (docker + dump real + chaves reais) expôs em
  minutos o que a suíte unitária não cobria: semântica de provedor real
  (temperature restritiva, 403 sobrecarregado). "Testar o teste" é barato e
  rendeu 2 bugs corrigidos + 3 candidatas de demanda.
- Especificação por status code é frágil quando o provedor sobrecarrega um
  código com semânticas distintas; classificar pelo corpo da resposta (como o
  routing engine já fazia via `ClassifyLimitLayer`) é o padrão — o key tester
  divergiu dele. Novos consumidores de erro upstream devem nascer
  classificação-por-corpo.
