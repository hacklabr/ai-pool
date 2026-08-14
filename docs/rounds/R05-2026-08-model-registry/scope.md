# Scope of round R05 — dicionário interno de especificações de modelos

> Epic: #26 · Variant: Minimal · Round R05 · 2026-08-14.

## Variant
minimal

## Requirements introduced
- (complemento da RF-11, sem RF novo): enriquecimento automático do catálogo
  a partir de dicionário EMBUTIDO no código (`internal/modelregistry/`,
  `specs.json` via embed.FS, zero dependências novas).

## Requirements changed
- (nenhuma — a RF-11 ganha bullets complementares no PRD descrevendo o
  dicionário e reafirmando a invariante).

## Requirements discontinued
- (nenhuma)

## Out of scope for this round
- Sync automático do dicionário a partir de fontes externas
  (models.dev/OpenRouter) — atualização é via PR.
- Pricing no dicionário.
- UI admin para editar o dicionário.

## Métricas de sucesso
Modelos do ambiente de teste (Z.ai glm-4.5..5.3, Kimi kimi-k3/kimi-k2.7-code
e aliases) ficam configurados sem intervenção manual: novos nascem com
context_window/output_limit/modalities; existentes com campo vazio recebem
backfill na primeira rodada; nada definido à mão é sobrescrito.

## Critérios de aceite (espelho da issue #26)
1. Modelo novo do upstream presente no dicionário nasce já com
   context_window/output_limit/modalities preenchidos.
2. Backfill preenche campos vazios de modelos existentes sem tocar em
   valores já definidos (garantido no SQL: `COALESCE`/`CASE`).
3. Modelo fora do dicionário mantém comportamento atual (só nome); sem
   registry injetado o sync é idêntico ao anterior.
4. Dicionário versionado no repo com os dados validados (embed, zero
   dependências novas); lookup por nome canônico e alias (herança + override
   parcial).
5. Relatório de sync contabiliza `enriched` por provedor (log, CLI, DTO
   admin).
6. `make ci-local` verde.
