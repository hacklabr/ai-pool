# Scope of round R02 — Estatísticas de plano das chaves de provedor

## Variant
minimal

## Contexto

Issue #17 (épico e tarefa única). Round de **investigação + plano** — sem
implementação. Os dois provedores usados pelo pool (Z.ai GLM Coding Plan e Kimi
Code/Membership) são planos de assinatura com cotas em janelas (5h / semanal /
mensal) ancoradas na data da assinatura, e **nenhum expõe API de cota/consumo**.

## Requirements introduced
<!-- RFs propostos pelo plano — só entram no PRD vivo na rodada de implementação -->
- RF-01 — Campos de plano na chave: `plan_name`, `plan_renewal_date`,
  `plan_quota_tokens` (denominador configurado pelo admin)
- RF-02 — % do plano consumido no ciclo (semanal/mensal) por soma local de
  tokens em `usage_logs`, ancorada em `plan_renewal_date`
- RF-03 — Log de ativação/desativação de chaves com mensagem de erro upstream
  sanitizada e camada do limite (5h/semanal/mensal/saldo/tier), usando a infra
  `audit_log` existente
- RF-04 — Indicador de subutilização no painel de chaves (thresholds
  configuráveis)
- RF-05 — Calibração empírica da cota: a cada desativação por teto, somar os
  tokens da janela e persistir como medição observada da cota (piso
  conservador), refinando o denominador ao longo dos ciclos

## Requirements changed
- (nenhum ainda — mudanças em `error_class` e `providerSlug` no wrapper de
  failover serão detalhadas no desenho técnico da rodada de implementação)

## Requirements discontinued
- (nenhum)

## Out of scope for this round
- Implementação de qualquer item (schema, coleta, UI) — vira demanda(s)
  filha(s) após aprovação do plano
- Provedores além de Z.ai (Coding Plan) e Kimi (Code/Membership)
- Polling de APIs de provedor (inexistente para esses planos — descartado na
  investigação)
- Kimi Open Platform (pay-as-you-go, `api.moonshot.*`) e Z.ai pay-as-you-go —
  sistemas independentes, sem ciclo de plano; o modelo "saldo" fica para
  demanda futura se o pool vier a usá-los
