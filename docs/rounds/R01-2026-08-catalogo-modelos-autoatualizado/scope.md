# Scope of round R01 — catálogo de modelos autoatualizado

> Epic: #18 · Variant: Minimal (modo direto, escopo ampliado por override registrado no épico) · Aprovado por @rafaelchavesfreitas em 2026-08-13.

## Variant
minimal

## Requirements introduced
- RF-11 — Sincronização automática do catálogo de modelos: job periódico que consulta o endpoint de listagem de modelos de cada provedor e reconcilia a tabela `models`; execução também no bootstrap/instalação via CLI idempotente `--sync-models`. Reconciliação fail-safe: provedor OK → adiciona novos (ativos), reativa retornantes, desativa ausentes; provedor indisponível → nenhuma alteração. Nunca deleta linhas; nunca sobrescreve enriquecimento manual (`context_window`, `model_pricing`, `modalities`, `model_aliases`). Intervalo via env `MODEL_SYNC_INTERVAL` (default 6h; `0` desativa).
- RF-12 — Listagem agregada de modelos: `GET /v1/models` (e `/api/models`) autenticado por token `sk-aipp-...`, formato OpenAI `{"object":"list","data":[...]}`, agregando modelos ativos de todos os provedores da organização.
- RF-13 — Script de atualização do cliente opencode: rota pública `GET /setup/update-models.sh` servindo script bash (`curl | bash`) que lê `~/.config/opencode/opencode.json`, extrai `apiKey`/`baseURL` do provider `llm-pool`, chama `GET /v1/models` e reescreve apenas a chave `models` desse provider, com backup prévio; requer `jq`.

## Requirements changed
- (nenhum)

## Requirements discontinued
- (nenhum)

## Out of scope for this round
- Sync de pricing/context window a partir de fontes externas (catálogos públicos).
- UI admin para disparar/visualizar o sync (CLI + logs bastam nesta round).
- Failover cross-provider por context window.
- Suporte a formatos de listagem de modelos não-OpenAI.
- Atualização do `mimocode.json` pelo script de update (o instalador/convite já cobre MiMo-Code).

## Métricas de sucesso
1. Modelo novo lançado por um provedor fica utilizável pelo pool sem cadastro manual, em no máximo 1 intervalo de sincronização.
2. Instalação nova do pool já nasce com o catálogo populado.
3. Usuário roda `curl | bash` e tem o `opencode.json` atualizado sem editar arquivo e sem digitar token.

## Critérios de aceite (espelho da issue #18)
1. Com provedor cadastrado e chave válida, rodar o sync faz os modelos dele aparecerem ativos e listados em `/v1/models` — sem cadastro manual.
2. Modelo que some da listagem fica inativo no próximo sync; se volta, reativa; nada é apagado.
3. Provedor fora do ar → sync não altera nada dele e o pool segue operando.
4. `curl -H "Authorization: Bearer sk-aipp-..." /v1/models` retorna a lista agregada; sem token, 401.
5. Instalação nova executa o sync automaticamente após as migrations.
6. Usuário com opencode configurado roda o script e o `opencode.json` fica com exatamente a lista vigente, sem perder outras configs e sem digitar token.
7. `make ci-local` verde.
