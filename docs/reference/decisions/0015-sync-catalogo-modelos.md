# ADR-0015 — Sincronização automática do catálogo de modelos

**Status:** proposto

**Contexto:** o catálogo de modelos (tabela `models`) era cadastrado exclusivamente
à mão: modelos novos dos provedores ficavam inacessíveis até alguém cadastrá-los,
e modelos descontinuados continuavam oferecidos (round R01, issue #18). Era
preciso decidir **onde** o sincronizador vive, **como** reconciliar a listagem
upstream com o cadastro local e **o que preservar** do enriquecimento manual
(`context_window`, `output_limit`, `modalities`, `model_pricing`, `model_aliases`
não existem na listagem upstream — o endpoint `GET {base_url}/models` devolve
apenas ids).

Alternativas consideradas:

1. **Cron externo / Job K8s separado** — rejeitada: mais um componente
   operacional para agendar, monitorar e versionar; o projeto já roda jobs
   periódicos in-process (padrão `usage.RollupJob`).
2. **Reconciliação destrutiva** (deletar modelos ausentes do upstream) —
   rejeitada: uma resposta transitória ruim do provedor destruiria cadastros
   enriquecidos manualmente; e modelos cadastrados à mão por decisão do admin
   (ex.: aliases legados) seriam removidos sem aviso.
3. **Sobrescrever todos os campos a cada sync** — rejeitada: apagaria o
   enriquecimento manual, que é a única fonte de `context_window`/pricing.

**Decisão:** vamos manter o sincronizador **in-process** (pacote
`internal/modelsync/`, goroutine + ticker, intervalo por
`AIPP_MODEL_SYNC_INTERVAL`, default 6h, `0` desativa), com execução adicional na
instalação via CLI idempotente `aipp --sync-models` (padrão `--migrate-only`,
chamado pelo Helm após as migrations). A reconciliação é **fail-safe**:

- provider indisponível/sem chave usável/resposta inválida → nenhuma alteração
  nos modelos dele (log sanitizado);
- provider OK → adiciona modelos novos (ativos, só com nome), reativa os que
  retornaram, **desativa** (`is_active=false`) os ausentes;
- **nunca deleta** linhas; **nunca sobrescreve** campos enriquecidos
  manualmente em modelos existentes.

**Consequências:**

- Positivas: catálogo se atualiza sozinho; instalação nasce populada; nenhum
  componente operacional novo; enriquecimento manual é investimento protegido;
  falha de provedor nunca corrompe o catálogo.
- Negativas: modelos descontinuados permanecem no banco como inativos (limpeza
  manual se um dia incomodar); atributos ricos de modelos **novos** continuam
  dependentes de cadastro manual (sync só traz o id — sync de pricing/contexto
  por fonte externa ficou fora de escopo).
- Fica mais fácil: expor `GET /v1/models` agregado (RF-12) e atualizar clientes
  via script (RF-13), pois ambos leem o catálogo local.

**Evidências:** `internal/modelsync/`, `cmd/aipp/main.go` (flag `--sync-models`
e wiring do job), `internal/config/config.go` (`AIPP_MODEL_SYNC_INTERVAL`),
`deploy/helm/ai-pool/templates/migration-job.yaml`.
