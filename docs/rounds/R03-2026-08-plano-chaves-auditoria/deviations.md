# Deviations of round R03 — Plano nas chaves e auditoria de transições

- **DEV-01 — providerSlug no chain em vez do wrapper.**
  Planejado: passar o slug real do provedor no wrapper de failover
  (`wrapper.go:89`, hoje hardcoded ""). Implementado: o slug vem de
  `resolution.Provider.Slug` nos call sites de `OnKeyFailed`/métricas dentro
  do chain, com fallback para `opts.ProviderSlug`. Motivo: o wrapper não
  conhece o slug antes de o motor resolver a chave — mesmo efeito, dados
  corretos. Documento de referência atualizado: descrição do PR #22
  (https://github.com/hacklabr/ai-pool/pull/22) documenta o desvio; o
  comentário de desenho da #17 é RECORD (imutável por classe).
- **DEV-02 — `error_class 'timeout'` não mapeado.**
  Planejado: mapear 429→rate_limit/quota, 401/403→auth, 5xx→5xx,
  timeout→timeout no `RecordProxyUsage`. Implementado: sem o caso timeout —
  timeouts produzem erro de rede (sem resposta upstream), não são observáveis
  nesse path. Mapeados: 429 (quota|rate_limit via camada), 401/403→auth,
  ≥500→5xx. Documento de referência atualizado: descrição do PR #22 (mesmo
  link acima) documenta o desvio.
