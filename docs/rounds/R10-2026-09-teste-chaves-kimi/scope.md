# Scope of round R10 — teste de chaves: request válido p/ Kimi + 403 concorrente sem auth_failed

> Epic: #33 · Variant: Minimal · 2026-09-28 · Evidência ao vivo: sessão de teste
> operacional (docker local, dump importado; provider kimi `api.kimi.com/coding/v1`).

## Variant
minimal

## Requirements introduced
- (nenhum — correção de comportamento já especificado)

## Requirements changed
- (correção RF-03, #33) **Teste de chave sob demanda** (`POST /keys/{id}/test`,
  `/keys/test-all`): body do teste não fixa mais `temperature` (campo omitido —
  provedor aplica o default; modelos de coding do Kimi rejeitam qualquer valor ≠ 1
  com HTTP 400 `invalid temperature`, impedindo a validação). No 403, a
  classificação passa a considerar o corpo da resposta (reutiliza a semântica do
  `routing.ClassifyLimitLayer`): limite concorrente/rate (ex.: "concurrent request
  limit") → erro transitório, NÃO marca `auth_failed`; 401 e 403 de auth real
  permanecem `auth_failed`; 429 permanece `quota_exceeded` com `Retry-After`.
  Comportamento exato do 403 transitório conforme decisão A/B (humano):
  (A) não altera status, apenas reporta [recomendada] · (B) desativa com
  `disabled_until` curto (~60s). PRD (RF-03) atualizado na mesma rodada.

## Requirements discontinued
- (nenhum)

## Out of scope for this round
- Alterar o motor de roteamento (`ClassifyLimitLayer` em si) — o key tester
  reutiliza, não modifica; reativar chaves desativadas na sessão de teste
  (operação de dados, não código); UI/UX do painel; parametrização de
  temperature por provider.

## Métricas de sucesso
Teste de chave Kimi no painel retorna veredito real da chave (200 ou erro real:
401/429/5xx — nunca 400 `invalid temperature`); 403 de limite concorrente não
desativa chave saudável. As 4 chaves Kimi do ambiente local passam a ser
validáveis pelo painel.

## Critérios de aceite
Espelhados na issue #33 (verificação: tabela de testes unitários do key tester +
`make ci-local` verde).

## Decisões
- **Pendente (humano):** 403 concorrente → (A) não altera status, reporta
  "limite transitório" · (B) desativa com `disabled_until` ~60s. Recomendação
  do facilitador: A (o limite indica outra request em curso — punir a chave é
  falso positivo). A definir antes do comentário técnico.
