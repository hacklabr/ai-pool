# Scope of round R03 — botão de sync no painel

> Epic: #25 · Variant: Minimal (modo direto) · Aprovado por @rafaelchavesfreitas em 2026-08-14.

## Variant
minimal

## Requirements introduced
- (nenhum RF novo — complemento operacional da RF-11): disparo manual do sync do catálogo pelo painel admin (rota `POST /api/web/admin/models/sync` + botão "Sincronizar agora" na página de modelos).

## Requirements changed
- (nenhum)

## Requirements discontinued
- (nenhum)

## Out of scope for this round
- Histórico de syncs na UI, progresso em tempo real por provedor, sync de provedor individual.
- Auditoria da ação manual de sync (o key tester grava audit_log; o sync manual não — follow-up se relevante).
- Lock contra disparo concorrente por dois admins (o modelsync é tolerante; refinamento futuro).

## Métricas de sucesso
Admin dispara a atualização sob demanda e vê o resumo (adicionados/reativados/desativados/ignorados) sem sair do painel nem esperar o job periódico (até 6h).

## Critérios de aceite (espelho da issue #25)
1. Botão visível para admin logado no header da página de modelos; dispara o sync sem recarregar a página.
2. Ao terminar, mostra o resumo na própria página.
3. Falha de provedor individual não retorna erro (fail-safe idêntico ao job); erro estrutural (DB) responde 5XX amigável.
4. Lista de modelos reflete o resultado do sync sem refresh manual.
5. `make ci-local` verde.
