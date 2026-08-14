# Scope of round R03 — Plano nas chaves e auditoria de transições

## Variant
minimal

## Contexto

Issue #19 (épico e tarefa única). Implementação da fundação do round R02:
campos de plano na chave, log de auditoria das 5 transições de estado com
mensagem de erro upstream sanitizada + camada do limite, e de-quebras
(`error_class`, `providerSlug` no failover). Absorveu a #12.

## Requirements introduced
- RF-01 — Campos de plano: `plan_name`, `plan_renewal_date`,
  `plan_quota_tokens` em `provider_keys` (migration 202608141000) + tabela
  `key_quota_measurements` + índice `usage_logs(key_id, created_at)`
- RF-03 — Eventos de auditoria (`entity_type='provider_key'`) nos 5 caminhos
  de transição, com `ClassifyLimitLayer` (Z.ai códigos 13xx / Kimi textos) e
  `SanitizeUpstreamMessage` (≤200 chars)
- RNF — de-quebras: `error_class` em erros upstream no usage log; slug real do
  provedor no failover (fecha #12)

## Requirements changed
- (nenhum)

## Requirements discontinued
- (nenhum)

## Out of scope for this round
- % do plano, calibração empírica e subutilização → #20 (round R04)
- Polling de API de provedor (inexistente para os planos usados)
