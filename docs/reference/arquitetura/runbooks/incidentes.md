# Runbook — Incidentes

> Criado em: 2026-08-13 · Última revisão: 2026-08-13
> Regra: doc desatualizado é corrigido ou marcado como obsoleto — nunca deixado apodrecendo em silêncio.

## Propósito

Resposta a incidentes em produção do AI Provider Pool.

## Pré-condições

- Acesso `kubectl` ao cluster e ao banco.
- Logs JSON sanitizados disponíveis (Loki/Grafana recomendado; `kubectl logs`
  como fallback).

## Sintomas × primeiras ações

| Sintoma | Verificar | Ação inicial |
|---|---|---|
| Todos os requests falham (5xx) | `/healthz`, `/readyz`, conectividade DB | DB down → proxy falha fechado por design (validação de token exige DB). Restaurar DB primeiro. |
| Muitas chaves `auto_disabled` | tela de Chaves na UI; métrica `aipp_keys_disabled_total` | Testar chaves (botão "testar"); verificar quota/billing nos provedores; force-enable se falso positivo. |
| `usage_logs_default` crescendo | partições existentes: `\d+ usage_logs` | Mês sem partição pré-criada — criar partição do mês corrente manualmente (modelo nas migrations) e mover dados da DEFAULT. |
| Streams cortados em deploy | annotations do Ingress (`proxy-buffering: off`), `preStopSleep` | Verificar template do Ingress; checar `SHUTDOWN_TIMEOUT`. |
| Suspeita de vazamento de segredo | logs (SanitizingHandler cobre padrões conhecidos) | Revogar token/chave afetado (revogação é imediata); rotacionar KEK se `provider_keys` comprometidas (procedimento em `internal/adapters/crypto/envelope.go`). |
| Dead-letter de usage crescendo | tabela `usage_logs_dead_letter` (`resolved_at IS NULL`) | Inserts de usage falhando — verificar pool de conexões e schema; reprocessamento manual (método `ReprocessDeadLetter` é stub — pendência). |

## Rollback deste runbook

Não se aplica (documento operacional).

## Pontos de atenção

- Fail-open conhecido: se a KEK falhar ao carregar, o app sobe com
  `IdentityDecryptor` (plaintext) e apenas loga erro — em produção, tratar log
  de erro de KEK como página crítica (dívida técnica #4 em `../../architecture.md` §7).
- Estado in-memory zera em restart: contadores de auto-disable, sessões e
  afinidades se perdem (por design, ADR-0001).
