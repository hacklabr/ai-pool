# Deviations of round R04 — Uso do plano por ciclo, calibração e subutilização

- **DEV-01 — Uma query por janela distinta, não por página.**
  Planejado: carregar o uso do plano de todas as chaves da página em UMA
  query. Implementado: uma query por janela de ciclo distinta (chaves têm
  âncoras de renovação diferentes; tipicamente 1 por tipo de ciclo). Sem N+1
  por chave. Documento de referência atualizado: descrição do PR #23
  (https://github.com/hacklabr/ai-pool/pull/23).
- **DEV-02 — Fix do teste de integração de migrations.**
  Fora do escopo planejado da #20, necessário para não deixar CI quebrado:
  `migrate_integration_test.go` esperava a versão da primeira migration e
  vinha falhando desde a R03 (migration 202608141000). Atualizado para
  esperar a última aplicada; verificado contra postgres:16 descartável
  (commit 93644cb, facilitador). Documento de referência atualizado: PR #23.
- **DEV-03 — Calibração dispara mesmo se o append de auditoria falhar.**
  Planejado: hook "no mesmo lugar do hook de auditoria". Implementado: path
  independente best-effort (ambos warn-only) — a medição não depende do
  sucesso da auditoria. Documento de referência atualizado: PR #23.
