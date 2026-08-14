# Scope of round R04 — Uso do plano por ciclo, calibração e subutilização

## Variant
minimal

## Contexto

Issue #20 (épico e tarefa única). Implementação da camada de estatísticas do
round R02 sobre a fundação da R03 (#19): % do plano consumido nos ciclos
semanal/mensal, calibração empírica da cota a cada teto atingido e indicador
de subutilização no painel de chaves.

## Requirements introduced
- RF-02 — % do plano por ciclo: helpers puros de janela
  (`internal/domain/plancycle.go`, clamp de dia, bissexto, virada) +
  `QueryKeyUsageInWindow` no índice `(key_id, created_at)`; UI sem N+1
- RF-04 — Flag "subutilizada": thresholds
  `AIPP_UNDERUTILIZATION_THRESHOLD_PCT` (30) /
  `AIPP_UNDERUTILIZATION_DAYS_BEFORE_RESET` (2)
- RF-05 — Calibração empírica: hook best-effort pós-auditoria no auto-disable
  grava medição em `key_quota_measurements`; denominador efetivo =
  MIN observado (calibrada) > plan_quota_tokens (cadastrada); UI explicita a
  confiança

## Requirements changed
- (nenhum)

## Requirements discontinued
- (nenhum)

## Out of scope for this round
- Polling de API de provedor (inexistente para os planos usados)
- Kimi Open Platform / Z.ai pay-as-you-go (modelos "saldo")
- Fix do `TestInvitationRepo_Accept*` (falha pré-existente, capturada como
  issue própria stage-0)
