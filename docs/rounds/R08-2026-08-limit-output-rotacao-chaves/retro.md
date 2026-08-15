# Retro of round R08 — limit.output + rotação de chaves

## Round signals
- **Specification gaps:** 1 — a R01 especificou o formato opencode sem conhecer a exigência de limit.output do OpenCode (validação descoberta em incidente).
- **New requirements discovered in Stage 3:** 1 (#30 rotação de chaves — nasceu do diagnóstico do incidente).
- **Late infeasibilities:** 0.
- **Documentation contradictions (`doc-bug`):** 0.
- **Registered overrides:** 0.
- **Feedback:** incidente relatado pelo autor com diagnóstico próprio (chave sem créditos).

## Process learnings
- Formatos de integração (schemas de terceiros) precisam de teste de validação CONTRA o consumidor real (o OpenCode rejeitou a config que nossos testes aprovavam).
- Falha 401 de uma chave ≠ falha do provedor: o pool de chaves existe para isso; o sync deveria ter girado desde a R01. Revisar outros consumidores de ListUsableByProvider que usam keys[0].
