# ROADMAP

> Itens **fora do escopo do MVP**, mas considerados nas decisões de arquitetura para
> facilitar a implementação futura. As decisões arquiteturais do MVP (ver
> [`DECISIONS.md`](DECISIONS.md)) já pavimentam o caminho para a maioria destes itens.

## Prioridade Alta (próximas iterações)

### 1. Opção B — Multi-réplica + Redis
Quando surgir: SLA duro, RPS sustentado > 50/s, quotas distribuídas ou multi-tenancy real.
- As interfaces de store (`LRUStore`, `AffinityStore`, `RateLimiter`, `TokenCache`) já existem.
- Adicionar implementação Redis (`go-redis`); mudar `replicas: 1` → `≥2`; ativar HPA/PDB.
- Ativar consistência de sessão (sticky sessions ou store compartilhado).

### 2. Endpoint `/metrics` Prometheus
- Instrumentation JÁ está no código (6 métricas).
- Só ativar `ENABLE_METRICS=true` + adicionar ServiceMonitor no Helm chart.

### 3. Particionamento automático de `usage_logs`
- CronJob Kubernetes para pré-criar partições mensais.
- Retention: DROP de partição aos 90 dias (revisar política LGPD).

### 4. Model aliasing/mapping
- Campo `model_aliases` (JSONB) já existe na tabela `models`.
- Implementar resolução de alias no routing (ex.: expor `gpt-4` → modelo interno).

### 5. Health check periódico das chaves (probe em background)
- Detectar chaves mortas antes de falhar na requisição real.
- Job periódico que prova cada chave habilitada.

## Prioridade Média

### 6. Times
- Tabelas `teams` + `team_memberships` (deferidas; `organization_id` já pavimenta).
- Cada team com acesso a uma coleção de provedores/chaves/modelos.

### 7. Quotas de utilização de tokens
- Por usuário/time em períodos de tempo (diário/semanal/mensal).
- `usage_logs` + rollups preparam a base de dados.

### 8. Limitação de horário por usuário
- Ex.: usuário X só pode usar entre 8h e 18h.

### 9. Multi-tenancy real (multiorganização)
- `organization_id NOT NULL DEFAULT 1` já em todas as tabelas.
- Implementar signup de organizações, isolamento, seletor de tenant na UI (slot já reservado no topbar).

### 10. Autenticação OAuth
- Configurável qual método usar (Google, GitHub, etc.).
- Login multi-step já desenhado para absorver sem redesign.

### 11. Notificação/webhook quando chave é desabilitada
- Visibilidade para admins (Slack, email, webhook genérico).

### 12. Cálculo/cadastro de custo por provedor/modelo
- Colunas `model_pricing` (nullable) já existem em `models`.
- Dashboard de custo.

### 13. Failover cross-provider
- Quando todas as chaves de um provedor esgotam, cair para outro provedor.
- **Atenção:** tamanho da janela de contexto varia entre provedores (risco de truncar histórico).

## Prioridade Baixa (visão de longo prazo)

### 14. Modelo "auto" (roteamento inteligente)
- Roteamento automático cross-provider/modelo por heurística.
- Ex.: arquivos anexados → modelo multimodal; requisições simples → modelo barato.
- A **arquitetura do routing engine como pipeline de políticas composável** (já implementada) prepara o terreno: cada heurística vira uma policy plugável.

### 15. Graceful shutdown já no MVP (concluído ✅)
- Movido do ROADMAP para o MVP por consenso dos especialistas.

### 16. mTLS interno
- Comunicação interna cifrada entre serviços.

### 17. KEK rotation automatizada
- Job de re-encriptação: lê cada chave, `Decrypt(KEK_antiga)` → `Encrypt(KEK_nova)` → UPDATE.
- Procedimento manual documentado em `internal/adapters/crypto/envelope.go`.

### 18. Cosign + SLSA (supply chain security)
- Assinatura de imagens; admission policy (Kyverno) verificando assinaturas.
- Placeholder já no workflow CI.

## Estado destes itens
Cada item tem **rastreabilidade** nas decisões arquiteturais do MVP ([`DECISIONS.md`](DECISIONS.md)) que o preparam. A coluna "preparação no MVP" indica o que já está pronto para acelerar cada item futuro.
