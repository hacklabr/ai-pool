# Scope of round R08 — limit.output sempre válido + rotação de chaves no sync

> Epics: #29, #30 · Variant: Minimal · 2026-08-15 · Incidentes reais em prod.

## Variant
minimal

## Requirements introduced
- (correção RF-13, #29) Lista servida em ?format=opencode é SEMPRE válida para o OpenCode: bloco limit completo (context+output) em todo modelo; nulos → defaults conservadores (context 128K, output 32K).
- (complemento RF-11, #30) Rotação de chaves no sync: 401/403 gira para a próxima chave usável do provedor até esgotar; falhas não-autorizacionais abortam.

## Requirements changed
- (nenhum)

## Requirements discontinued
- (nenhum)

## Out of scope for this round
- Validador de schema no script bash; defaults configuráveis por env; auto-disable da chave no sync (hot path já cuida); alias "k2.7" documentado no specs.json (feito).

## Métricas de sucesso
Rodar /setup/update-models.sh nunca produz opencode.json rejeitado; chave sem crédito não impede o sync quando há outra válida.

## Critérios de aceite
Espelhados nas issues #29 e #30 (verificados: TestOpenCodeModelsMap, TestListModels_OpenCodeFormat, TestSyncAll_KeyRotationOn401 — 3 subtestes).
