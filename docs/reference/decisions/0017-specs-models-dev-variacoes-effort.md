# ADR-0017 — Especificações de modelos via models.dev e variações de reasoning effort

**Status:** aceito

**Contexto:** O dicionário embutido `internal/modelregistry` (issue #26, ADR-0015)
exigia PR no ai-pool para cada modelo novo/especificação alterada (context_window,
output_limit, modalidades), e não cobria as "variações" dos modelos (max/high/low)
que os clientes esperam alternar. Pesquisa (2026-08-15) confirmou:

1. As variações NÃO são ids distintos nos upstreams Z.ai/Kimi — são o parâmetro
   `reasoning_effort` do request (Kimi K3: low/high/max; Z.ai: low/high/max com
   auto-conversão de aliases).
2. O OpenCode expõe isso nativamente como `variants` no config do provider
   (`"variants": {"max": {"options": {"reasoningEffort": "max"}}}`), enviado
   pelo `@ai-sdk/openai-compatible` como `reasoning_effort` no body.
3. `https://models.dev/api.json` — a mesma fonte que o OpenCode usa para
   providers built-in — mantém, por modelo: `limit {context, output}`,
   `modalities {input, output}` e `reasoning_options` (valores de effort).

**Decisão:**

1. Substituir o dicionário embutido pelo fetch do catálogo público models.dev
   (novo pacote folha `internal/modelsdev`, só stdlib), feito 1x por rodada de
   sync (job periódico, CLI `--sync-models` e botão admin). Lookup GLOBAL por
   model-id com prioridade para provedores de primeira parte (`zai*`, `kimi*`,
   `moonshotai*`) sobre agregadores que listam os mesmos ids.
2. Persistir os efforts em `models.reasoning_options jsonb` (backfill segue a
   invariante do ADR-0015: só preenche vazio, nunca sobrescreve).
3. Expor as variações ao cliente como `variants` + `reasoningEffort` no mapa
   de modelos do OpenCode (`modelcatalog`), consumido pelo endpoint agregado
   `GET /v1/models?format=opencode` e pelo script de setup de convite (que já
   injeta `.models` inteiro — herdou as variants sem mudança).
4. Fetch do catálogo falho NÃO é estrutural: sync segue sem enriquecimento
   (warn), o próximo tick tenta de novo — fail-safe do ADR-0015 estendido.

**Consequências:**
Positivas: cobertura de novos modelos sem PR no ai-pool; variações de effort
disponíveis no picker do OpenCode sem IDs sintéticos (o proxy permanece
byte-passthrough — regra inviolável preservada); menos conteúdo versionado
no repo (specs.json deletado).
Negativas: dependência de rede/terceiro no sync (mitigada pelo fail-safe e
pelo fato de o catálogo local já persistido não ser esvaziado); qualidade do
catálogo models.dev é comunitária (para modelos de primeira parte Z.ai/Kimi
os dados conferem com as docs oficiais; divergências em existentes NÃO são
corrigidas automaticamente — invariante do backfill).

**Evidências:**
`internal/modelsdev/` (client + índice); `internal/modelsync/` (SpecSource
nova assinatura com reasoningEfforts); `migrations/202608151606_add_models_reasoning_options.*.sql`;
`internal/adapters/postgres/model_repo.go` (coluna + BackfillSpecs);
`internal/adapters/httpserver/modelcatalog/` (variants); `cmd/aipp/main.go`
(wiring). Substitui o dicionário do ADR-0015 (issue #26).
