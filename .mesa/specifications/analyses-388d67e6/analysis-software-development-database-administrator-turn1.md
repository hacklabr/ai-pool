# Analysis: Database Administrator — Turn 1

**Agent ID:** software-development-database-administrator
**Timestamp:** 2026-06-25T18:48:29.779Z

# Análise DBA — AI Provider Pool (Go + Beego + PostgreSQL)

## 1. Problem Statement (âmbito de dados)

Modelar o schema relacional do proxy OpenAI-compatible priorizando: (a) validação de token O(1); (b) alto volume de logs de uso sem degradação; (c) rastreabilidade de estado de chaves para o scheduler de reativação; (d) extensibilidade barata agora para absorver o roadmap (multi-tenancy, teams, quotas, aliases, cost tracking) sem migrações pesadas depois; (e) segurança (credenciais hasheadas/encriptadas, least-privilege, sem plaintext).

Stack confirmada no briefing: **PostgreSQL** (não dizer MySQL/PlanetScale). ~30 usuários, ~5 provedores, 1–5 chaves por provedor. Volume de logs é o ponto de atenção — não o número de chaves.

Princípio adotado: **schema é contrato** — normalizar primeiro, desnormalizar só quando profiling justificar. **Medir antes de otimizar.** Como projeto greenfield sem cargas reais, propõe-se uma baseline sane com indexes justificados e revisão via `EXPLAIN ANALYZE` assim que houver tráfego.

---

## 2. Modelo de dados — MVP

Convenções globais sugeridas:
- Todas as PKs: `id BIGINT GENERATED ALWAYS AS IDENTITY` (uniforme, join-friendly; UUID opcional como coluna pública `public_id UUID` onde fizer sentido expor).
- Todas as tabelas ganham `created_at TIMESTAMPTZ NOT NULL DEFAULT now()`, `updated_at TIMESTAMPTZ`, e `deleted_at TIMESTAMPTZ NULL` (soft-delete para auditoria — nunca `DELETE` físico em usuários/tokens).
- FKs terminadas em `_id` são `BIGINT`. Toda FK recebe índice.
- Encoding/cluster: `UTF8`, timezone armazenado sempre em `TIMESTAMPTZ` (UTC).

### 2.1 RBAC — `users`, `roles`

Decisão-chave: **tabela `roles` desde já**, mesmo o MVP tendo 2 papéis (`admin`, `user`). Custo de criar a tabela é zero; custo de migrar `users.role` (ENUM) → tabela relacional depois é alto (rewrite de queries, dados, permissões). Seed com 2 registros.

- `roles(id, name UNIQUE, description, is_system BOOL DEFAULT false)` — `is_system` impede excluir papéis núcleo.
- `users(id, public_id UUID, username CITEXT UNIQUE NOT NULL, email CITEXT UNIQUE NULL, password_hash TEXT NOT NULL, password_algo TEXT DEFAULT 'argon2id', role_id BIGINT NOT NULL REFERENCES roles(id), status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled','deleted')), failed_login_count INT DEFAULT 0, last_login_at TIMESTAMPTZ, timestamps, deleted_at)`
  - `password_hash` sempre Argon2id (ou bcrypt mínimo). Nunca plaintext, nunca MD5/SHA puro.
  - `status='disabled'` → perde acesso ao pool imediatamente (atende requisito "desabilitar/excluir usuário perde acesso"). `status='deleted'` = soft-delete.
  - UNIQUE em `username` deve ser **partial unique index** com `WHERE deleted_at IS NULL` para permitir reuso de nome após soft-delete.

**Extensibilidade (freebie):** `permissions(id, name)` + `role_permissions(role_id, permission_id)` podem esperar — só criar quando surgir granularidade real. A tabela `roles` já abre caminho.

### 2.2 `organizations` (multi-tenancy desde o dia 1 — quase grátis)

**Recomendação forte:** criar `organizations(id, name, slug UNIQUE, status, timestamps)` com **1 registro seed** `id=1, slug='default'`. Toda tabela multitenant ganha `organization_id BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id)`.

- Custo agora: 1 coluna + FK por tabela (~nenhum).
- Custo depois (sem isso): migrar N tabelas grandes (especialmente `usage_logs` particionada) para adicionar coluna + backfill — caro e arriscado.
- Quando o roadmap "multiorganização" chegar, é só criar novos registros em `organizations`; o schema já está pronto. O índice deve ser **composite** com `organization_id` à esquerda em toda busca tenant-scoped.

Se houver objeção a "premature", o segundo melhor caminho é **deixar `organization_id BIGINT NULL`** com comentário e índice, populando tudo com `1` apenas quando o recurso chegar. Mas recomendo NOT NULL desde já.

### 2.3 `api_tokens` (tokens dos usuários)

- `api_tokens(id, user_id BIGINT NOT NULL REFERENCES users(id), name TEXT, token_hash BYTEA UNIQUE NOT NULL, token_prefix TEXT NOT NULL, -- ex.: 'sk-abc...' máscara para UI ('sk-abcd…1234')
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked','expired')), expires_at TIMESTAMPTZ NULL, last_used_at TIMESTAMPTZ, last_used_ip INET NULL, created_by_user_id BIGINT, timestamps, deleted_at)`

**Crítico de segurança:** **NUNCA persistir o token em plaintext.** Guardar `token_hash = SHA-256(token)` (32 bytes como `BYTEA`). Como o token (`sk-...` de alta entropia gerado CSPRNG) já tem entropia suficiente, **SHA-256 determinístico** basta para lookup O(1) — não precisa de Argon2/bcrypt aqui (que são lentos e impossibilitariam lookup direto). No login do request: `WHERE token_hash = digest($1, 'sha256')`.

- `UNIQUE` em `token_hash` → validação O(1) garantida (índice B-tree, busca por igualdade). Atende requisito explícito do briefing.
- O token completo é mostrado **uma única vez** ao gerar; depois só `token_prefix` (4–8 chars) na UI.
- `revoked` (admin/user revoga) e `expired` (por `expires_at`) são estados distintos — token revogado é irrevogável.
- `last_used_at`/`last_used_ip` — gravar periodicamente (debounce de ~60s, ver §7), não a cada request (write amplification).

### 2.4 `providers` e `models`

- `providers(id, organization_id BIGINT NOT NULL DEFAULT 1, name TEXT NOT NULL, slug TEXT NOT NULL, base_url TEXT NOT NULL, api_spec TEXT NOT NULL DEFAULT 'openai', -- 'openai'|'gemini'|'anthropic'... facilita parsers
  user_agent_override TEXT NULL, -- override global do provedor
  auth_scheme TEXT DEFAULT 'bearer', is_active BOOL DEFAULT true, timestamps)`
  - UNIQUE `(organization_id, slug)`.
  - `api_spec` já abstrai a heterogeneidade dos provedores (Gemini/Mistral/etc.) — pavimenta "parsers por provedor" e "auto routing".

- `models(id, provider_id BIGINT NOT NULL REFERENCES providers(id), name TEXT NOT NULL, -- nome real no provedor
  display_name TEXT NULL, context_window INT NULL, is_active BOOL DEFAULT true, timestamps)`
  - UNIQUE `(provider_id, name)`.
  - `context_window` é útil **hoje** para o roadmap de failover cross-provider (alertar sobre janela de contexto divergente) — vale persistir já (campo barato).

### 2.5 `model_aliases` (roadmap aliasing — criar desde já, ~grátis)

- `model_aliases(id, organization_id BIGINT NOT NULL DEFAULT 1, alias TEXT NOT NULL, -- ex.: 'gpt-4'
  target_model_id BIGINT NULL REFERENCES models(id), -- referência; OU fallback abaixo
  target_provider_id BIGINT NULL, target_model_name TEXT NULL, -- quando quiser mapear sem model cadastrado
  priority INT DEFAULT 0, is_active BOOL DEFAULT true, timestamps)`
  - UNIQUE `(organization_id, alias)`.
  - O proxy, ao receber `model='gpt-4'`, resolve alias → modelo real. Criar a tabela hoje evita migração no futuro; o MVP pode simplesmente não popular.

### 2.6 `model_pricing` (roadmap cost tracking — criar desde já, nullable)

- `model_pricing(id, model_id BIGINT NOT NULL REFERENCES models(id), input_price_per_1k NUMERIC(12,6) NULL, output_price_per_1k NUMERIC(12,6) NULL, cached_input_price_per_1k NUMERIC(12,6) NULL, -- cache de contexto (importante p/ Gemini)
  currency CHAR(3) DEFAULT 'USD', effective_from DATE NOT NULL, effective_until DATE NULL, timestamps)`
  - PK composta alternativa: `(model_id, effective_from)`. Permite histórico de preço.
  - Tudo nullable → no MVP só existe a estrutura; quando o cost tracking chegar, é só popular.

### 2.7 `provider_keys` (chaves dos provedores — coração do sistema)

- `provider_keys(id, provider_id BIGINT NOT NULL REFERENCES providers(id), label TEXT NOT NULL, encrypted_value BYTEA NOT NULL, -- AES-GCM ciphertext
  encryption_key_id TEXT NOT NULL DEFAULT 'v1', -- p/ rotação de chave
  status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled','disabled','auto_disabled')), disabled_reason TEXT NULL CHECK (disabled_reason IN ('manual','quota_exceeded','rate_limited','error_threshold','auth_failed','parse_failed_retry')), disabled_at TIMESTAMPTZ NULL, auto_reactivate_at TIMESTAMPTZ NULL, -- scheduler lê isto
  error_count INT NOT NULL DEFAULT 0, error_window_started_at TIMESTAMPTZ NULL, -- janela deslizante p/ auto-disable
  consecutive_errors INT NOT NULL DEFAULT 0, last_error_at TIMESTAMPTZ NULL, last_error_message TEXT NULL, last_error_provider_hint TEXT NULL, -- data parseada do erro (cota libera em...)
  last_used_at TIMESTAMPTZ NULL, -- LRU fallback/persistência entre restarts
  user_agent_override TEXT NULL, -- override por chave (sobrepõe provider)
  timestamps, deleted_at)`

**Segurança:**
- `encrypted_value` via **AES-256-GCM**: guarda ciphertext + nonce/IV + tag. Chave vem de K8s Secret/Vault (env `PROVIDER_KEY_ENCRYPTION_KEY`), nunca no repositório.
- `encryption_key_id` permite **rotação**: nova chave = `v2`; re-encriptar lazy ou por job. Barato adicionar agora.
- Aplicação descriptografa **apenas no momento do request ao provedor**, nunca loga, nunca retorna na API/JSON.

**Estado da chave (atende requisitos 4, 8 e scheduler):**
- `status` enum de 3 valores é suficiente e expressivo. `auto_disabled` distingue de `disabled` manual — o scheduler **só** reativa `auto_disabled`.
- `auto_reactivate_at` é o campo que o scheduler consulta (ver §4.2).
- `disabled_reason` discrimina causa → útil para dashboard/auditoria e para decisão de reativação.
- `error_count`/`error_window_started_at` modelam **janela deslizante** (ex.: ≥5 erros em 60s → auto-disable). Resetar a cada janela.
- `consecutive_errors` para critério rápido "N falhas consecutivas" (reset em sucesso).
- `last_error_provider_hint` guarda o timestamp extraído do parse da mensagem — é a **fonte** de `auto_reactivate_at` quando o parse funciona; quando falha, `auto_reactivate_at = now() + backoff`.

**Índices (justificados):**
- `(provider_id, status, last_used_at)` — para seleção LRU de chave ativa de um provedor (ORDER BY last_used_at NULLS FIRST).
- **Partial index** `CREATE INDEX ... ON provider_keys (auto_reactivate_at) WHERE status='auto_disabled' AND auto_reactivate_at IS NOT NULL` — consulta do scheduler fica minúscula.
- `UNIQUE(provider_id, label)` (parcial `WHERE deleted_at IS NULL`).

### 2.8 `rate_limits` (configuração — runtime fica em cache)

- `rate_limits(id, organization_id BIGINT NOT NULL DEFAULT 1, scope_type TEXT NOT NULL CHECK (scope_type IN ('global','organization','team','user','token')), scope_id BIGINT NULL, -- null p/ global/organization; id da entidade p/ os demais
  requests_per_minute INT NULL, requests_per_hour INT NULL, requests_per_day INT NULL, is_active BOOL DEFAULT true, timestamps)`
  - UNIQUE `(organization_id, scope_type, scope_id)`.

**Importante (separa config vs runtime):** a *configuração* mora aqui; a *contagem efetiva* de janela deslizante **não** deve ser feita em Postgres por request (locks/updates por linha a cada hit). No MVP usar contador **in-memory** (mapa por token, com TTL) no processo Go; no roadmap, **Redis** (sorted set) — pavimenta observabilidade e K8s com réplicas. Documentar esta separação explicitamente no spec.

### 2.9 `usage_logs` — **particionada, alto volume** (ver §3)

### 2.10 `usage_rollups` (agregados para dashboard — ver §3.4)

### 2.11 `audit_log` (configuração mutável, low-volume)

- `audit_log(id, organization_id, actor_user_id NULL, action TEXT, -- 'provider_key.disable', 'user.update'...
  entity_type TEXT, entity_id BIGINT, before JSONB NULL, after JSONB NULL, ip INET, user_agent TEXT, created_at)`
- Captura *quem alterou o quê* em config (admin force-disable, criar/revogar token, alterar rate limit). Não particionar no MVP (baixo volume), mas planejar particionamento mensal se crescer.

---

## 3. Logs de uso em ALTO VOLUME — estratégia

Cada request gera 1 linha. Mesmo com 30 usuários, se cada um fizer centenas de requests/dia (chat é request-heavy com streaming), chegamos facilmente a **dezenas de milhares de linhas/dia** → **milhões/ano**. Seq scan virará inimigo. Estratégia em 4 camadas:

### 3.1 Particionamento declarativo por mês

```sql
CREATE TABLE usage_logs (
  id            BIGINT GENERATED ALWAYS AS IDENTITY,
  request_id    UUID NOT NULL,          -- correlation/tracing
  organization_id BIGINT NOT NULL,
  user_id       BIGINT NOT NULL,
  token_id      BIGINT NOT NULL,
  provider_id   BIGINT NOT NULL,
  provider_key_id BIGINT NOT NULL,
  model_name    TEXT NOT NULL,
  model_id      BIGINT NULL,            -- FK lógica (não pode ser FK dura em particionada)
  stream        BOOL NOT NULL DEFAULT false,
  prompt_tokens     INT NULL,
  completion_tokens INT NULL,
  total_tokens      INT NULL,
  cached_tokens     INT NULL,           -- p/ cache de contexto (custo/insight)
  latency_ms    INT NULL,
  status_code   INT NULL,
  error_class   TEXT NULL,              -- 'rate_limit','auth','timeout','5xx','quota'
  client_ip     INET NULL,
  client_ua     TEXT NULL,
  cost_usd      NUMERIC(10,6) NULL,     -- p/ cost tracking (nullable agora)
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (id, created_at)          -- PK deve conter a chave de particionamento
) PARTITION BY RANGE (created_at);
```

- **PK composta `(id, created_at)`** é exigência do PostgreSQL em tabelas particionadas (a partição deve ser determinável pela PK).
- FKs **não podem** apontar para tabela particionada em PG < 19; e mesmo onde podem, o custo é alto. Referenciar `provider_id`/`user_id` **logicamente** (sem CONSTRAINT FK física), confiando na integridade da aplicação. É o trade-off aceito do particionamento temporal. Documentar.

### 3.2 Criação automática de partições

- **Não** criar partições manualmente. Usar **`pg_partman`** (extensão madura) com `pg_cron`/job externo, **OU** um **CronJob no K8s** que roda uma migration aplicação-side criando a partição do próximo mês + um buffer (ex.: criar sempre os próximos 2 meses).
- Política: **partições mensais**. Janela de particionamento suficiente para ~30 usuários (partição mensal < 5M linhas em cenário pessimista).
- **Pre-partition**: nunca deixar faltar partição futura → inserts falham. Monitorar "missing partition" alert.

### 3.3 Retention

- **Granular:** manter 90 dias de `usage_logs` (configurável). Drop de partição inteira (`DROP TABLE usage_logs_2026_03`) é **instantâneo** e não gera DEAD tuples — é a grande vantagem do particionamento para retention.
- **Agregados:** `usage_rollups` (ver §3.4) mantidos por **anos** (baixo volume) — histórico de longo prazo do dashboard.
- Nunca `DELETE` por linha para limpeza — sempre `DROP` de partição.

### 3.4 Agregação / materialização para o dashboard

Duas tabelas de rollup (preferidas a `MATERIALIZED VIEW` por permitirem **incremental insert** sem `REFRESH` full):

- `usage_rollups_hourly(organization_id, user_id, provider_id, model_name, bucket_hour TIMESTAMPTZ, request_count BIGINT, prompt_tokens BIGINT, completion_tokens BIGINT, total_tokens BIGINT, error_count BIGINT, avg_latency_ms NUMERIC, PRIMARY KEY (organization_id, user_id, provider_id, model_name, bucket_hour))`
- `usage_rollups_daily(... bucket_date DATE ...)`

Job (pg_cron ou aplicação): a cada hora, agregue a última hora fechada de `usage_logs` e faça `INSERT ... ON CONFLICT (...) DO UPDATE SET ...` (upsert idempotente) nas rollups. Dashboard consulta **somente rollups** → queries de ms.

Afinidade com cache de contexto (requisito 2 do briefing): a coluna `cached_tokens` captura o ganho — útil para o dashboard mostrar "economia" gerada pela estratégia de roteamento.

### 3.5 Insert de log sem bloquear o path do request

- **Insert assíncrono/buffered**: o request do usuário **não pode** esperar o INSERT do log finalizar para responder. Padrão: enfileirar em **canal Go (channel buffered)** e um worker drena em **batches** (`INSERT ... VALUES (...),(...),(...)` de 100–500 linhas). Em pico, descarta logs com backpressure (logging é best-effort), mas registra métrica de drops.
- Alternativa robusta no roadmap: escrever logs em fila (Redis Stream / NATS / Kafka) e consumir. Para o MVP, batch-in-app é suficiente.

---

## 4. Indexação (cada índice justificado)

### 4.1 Validação de token (O(1))
- `api_tokens`: `UNIQUE(token_hash)` — B-tree, lookup exato. **O(1) prático** (log N, mas N pequeno e cacheado). Adicionalmente, **partial index** `WHERE status='active' AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at > now())` se quiser filtrar na própria busca; senão filtra na aplicação após o SELECT.

### 4.2 Scheduler de reativação (query frequente, baixa cardinalidade)
- **Partial index** em `provider_keys(auto_reactivate_at) WHERE status='auto_disabled' AND auto_reactivate_at IS NOT NULL`. A query `WHERE status='auto_disabled' AND auto_reactivate_at <= now() FOR UPDATE SKIP LOCKED LIMIT N` fica sub-milissegundo.

### 4.3 LRU de chaves
- `provider_keys(provider_id, status, last_used_at NULLS FIRST)` — `ORDER BY last_used_at NULLS FIRST LIMIT 1 WHERE provider_id=? AND status='enabled'`. Chaves nunca usadas (NULL) vêm primeiro — desejável para distribuir cold-start.

### 4.4 Dashboard (logs por usuário/provedor/período)
- Como a tabela é particionada por mês, o **prune de partição** já corta ~90% dos dados quando o filtro é por período. Sobre cada partição, índices:
  - `(organization_id, user_id, created_at DESC)` — uso por usuário ao longo do tempo.
  - `(organization_id, provider_id, created_at DESC)` — uso por provedor.
  - `(organization_id, created_at DESC)` — visão geral.
- **Atenção:** índices em particionada são por-partição (PostgreSQL 11+ propaga para partições, mas o custo de manter N índices existe). Revisar via `pg_stat_user_indexes` e `DROP` índices não usados após profiling real.

### 4.5 Consultas "menos usada recentemente" persistida
- Se a estratégia LRU for híbrida (in-memory + persistência `last_used_at` em `provider_keys`), o índice de §4.3 basta. **Não** consultar logs para LRU — caro e desnecessário. Logs são para **auditoria/dashboard**, não para seleção de chave em runtime.

### 4.6 Cobertura
- Para a query "tokens ativos de um usuário" na UI: covering index `api_tokens(user_id) INCLUDE (name, token_prefix, status, last_used_at) WHERE deleted_at IS NULL` — evita heap fetch.

---

## 5. Estado das chaves — modelagem do scheduler

### 5.1 Máquina de estados (declarada via CHECK)

```
enabled ──(admin force)──────────────────────► disabled (manual; só admin reativa)
enabled ──(threshold erro/parse de data)─────► auto_disabled (auto_reactivate_at set)
disabled ──(admin force)──────────────────────► enabled
auto_disabled ──(scheduler, auto_reactivate_at <= now())──► enabled
```

- `disabled` (manual) e `auto_disabled` são estados distintos: **apenas `auto_disabled`** é reativável pelo scheduler.
- `disabled_reason` documenta o porquê — essencial para auditoria e diagnóstico.

### 5.2 Query do scheduler (rodar a cada 10–30s)

```sql
-- Pegar chaves prontas para reativação, sem disputa entre workers
SELECT id, provider_id, auto_reactivate_at
FROM provider_keys
WHERE status = 'auto_disabled'
  AND auto_reactivate_at IS NOT NULL
  AND auto_reactivate_at <= now()
ORDER BY auto_reactivate_at
FOR UPDATE SKIP LOCKED   -- múltiplos workers não pegam a mesma
LIMIT 50;
-- depois: UPDATE ... SET status='enabled', auto_reactivate_at=NULL, error_count=0,
--        consecutive_errors=0, disabled_reason=NULL WHERE id = ANY(...);
```

- `FOR UPDATE SKIP LOCKED` resolve concorrência sem race.
- Antes de marcar `enabled`, o scheduler pode opcionalmente fazer um **probe** (health check — roadmap) para validar; no MVP, confiar na data.

### 5.3 Auto-disable atômico (vários workers detectam o mesmo erro)

```sql
UPDATE provider_keys
SET status='auto_disabled',
    disabled_reason='quota_exceeded',
    disabled_at=now(),
    auto_reactivate_at=:parsed_or_backoff
WHERE id = :key_id AND status IN ('enabled','disabled');
-- RETURNING id;  -- só 1 worker "vence", outros não causam dano
```
Idempotente: a condição `status IN ('enabled','disabled')` + atomicidade do UPDATE evita dupla transição.

---

## 6. Extensibilidade para o ROADMAP — o que adicionar AGORA (barato) vs depois

### 6.1 Adicionar agora (custo ~zero, evita migração cara)

| Item | Ação agora | Justificativa |
|---|---|---|
| Multi-tenancy | `organizations` seed (id=1) + `organization_id BIGINT NOT NULL DEFAULT 1` em **todas** as tabelas multitenant + índices composite começando por `organization_id` | Migrar depois = backfill em `usage_logs` particionada = pesadíssimo |
| Teams | Criar `teams(id, organization_id, name, slug)` + `team_members(team_id, user_id, role, joined_at)` vazias | Tabela nova agora ou depois é o mesmo esforço; criar deixa o modelo "completo" |
| Quota de tokens | `quotas(id, organization_id, scope_type, scope_id, tokens_per_day BIGINT, tokens_per_month BIGINT, reset_day INT, is_active)` | `usage_logs` já tem `total_tokens`; o enforcement vem depois |
| Limite de horário | `time_window_limits(quota_id FK ou scope...)` ou simplesmente coluna `allowed_hours JSONB` em `quotas` | JSONB flexível, sem schema rígido |
| Model aliasing | `model_aliases` (ver §2.5) | Tabela nova, barata |
| Cost tracking | `model_pricing` (§2.6) + coluna `cost_usd NULL` em `usage_logs` | Coluna nullable em tabela particionada adicionada no boot não dobra custo |
| Rotação de chave de encriptação | `encryption_key_id` em `provider_keys` (§2.7) | Sem isso, rotação futura é operacionalmente complexa |
| Webhook de disable (roadmap) | Coluna `webhook_url NULL` em `organizations` + flag `notify_on_key_disable BOOL` | Nullable, sem custo |
| `cached_tokens` em logs | Coluna nullable já | Insight do cache de contexto (diferencial do produto) |

### 6.2 Adicionar DEPOIS (migração inevitável, OK deixar)

- **OAuth/SSO** → tabela `user_identities(user_id, provider, subject, raw JSONB)` — só quando OAuth chegar; modelo de identidade externa é específico demais para prematurizar.
- **Failover cross-provider** → é **lógica de aplicação**, não schema. `models.context_window` já está lá para suportar a checagem.
- **Model "auto" routing** → lógica/configuração; pode usar `model_aliases` como base.
- **Health check periódico** → tabela `provider_key_health_checks` nova, só quando implementar probes.
- **Métricas Prometheus** → endpoint, sem schema.

### 6.3 Regra geral
**Toda coluna/relação que toca `usage_logs` (particionada) ou `provider_keys` (alto churn) deve ser adicionada agora.** Tabelas novas isoladas podem esperar.

---

## 7. Concorrência e consistência

### 7.1 Atualização de estado de chave (alto churn)
- Sempre via `UPDATE ... WHERE id=? AND <condição de estado>` **atômica** (lock pessimista implícito da linha). Evita locks explícitos `SELECT FOR UPDATE` desnecessários.
- **`FOR UPDATE SKIP LOCKED`** para filas (scheduler de reativação, fila de logs se distribuído).
- **Não usar** `SELECT FOR UPDATE` seguido de `UPDATE` para "decidir e atualizar" estado de chave — há janela de race. Fazer o `UPDATE ... RETURNING` condicional.

### 7.2 Locks otimistas para config administrativa (UI)
- `providers`, `rate_limits`, `model_aliases`, `provider_keys` (campos de config) editados por admins via UI: adicionar coluna `version INT NOT NULL DEFAULT 1` e validar `UPDATE ... SET version=version+1 WHERE id=? AND version=:expected`. Evita lost-update quando dois admins editam simultaneamente.
- Mais leve que `FOR UPDATE` para sessões web de longa duração.

### 7.3 `last_used_at` de tokens e chaves (write amplification)
- Atualizar `last_used_at` a **cada request** = write por request na mesma linha = contenção (HOT update alivia, mas ainda é custoso).
- **Debounce na aplicação**: atualizar no máximo a cada 60s por entidade (in-memory "dirty set"), flush em batch. Aceita-se pequena imprecisão (≤60s) em troca de MUITO menos writes.

### 7.4 Insert de log
- Isolado por transação curta, em batch (§3.5). Não bloqueia path crítico.
- `synchronous_commit` pode ser `off` ou `local` para a conexão de logging (se tolerar perda de último log em crash) — ganho de latência. Avaliar via profiling; default manter `on`.

### 7.5 Isolamento
- `READ COMMITTED` (default) é suficiente para o caso. Não há necessidade de `SERIALIZABLE` — custaria throughput.

---

## 8. Migrations — estratégia e ferramenta

### 8.1 Ferramenta recomendada: **golang-migrate**
- Beego ORM tem migrations, mas **limitadas** (sem down robusto, menos comunidade).
- **golang-migrate** é o padrão de facto em Go: arquivos `NNNNNN_name.up.sql` / `.down.sql` versionados no git; suporta embed (`embed.FS`) para empacotar no binário; CLI + library; múltiplos drivers (PG nativo).
- Alternativas válidas: **goose** (similar), **atlas** (mais declarativo, schema-as-code). Para o contexto (equipe pequena, K8s), golang-migrate é o ponto ótimo.

### 8.2 Disciplina
- **Toda migração reversível** (down funcional). Sem down = bloqueia rollback.
- **Sem DDL destrutivo em up**: `DROP COLUMN` vira "renomear + deixar nullable + remover depois". `DROP TABLE` só após janela de depreciação.
- **Sem `ACCESS EXCLUSIVE` em horário comercial**: usar `CREATE INDEX CONCURRENTLY`, `ALTER TABLE ... ADD COLUMN ... DEFAULT ... NOT NULL` em 2 passos (add nullable → backfill → set NOT NULL VALIDATE) para tabelas grandes.
- **Migrações aplicadas no boot do pod** (Job initContainer ou hook) **OU** via pipeline CI/CD antes do deploy — nunca manualmente em produção.
- Tabela `schema_migrations` gerenciada pela ferramenta — não mexer.
- Cada migração revisada em PR; testada em staging primeiro.

### 8.3 Versionamento
- Commits de migração atômicos com a feature que os introduz.
- Convenção de nome: `YYYYMMDDHHMM_description`.
- Manter changelog/migration README explicando decisões.

---

## 9. Backup e durabilidade

### 9.1 Desenvolvimento (Docker Compose)
- Volume Docker persistente para `PGDATA`. `pg_dump` lógico agendado (cron no host) para snapshot rápido.
- Não há SLA; o foco é reprodutibilidade.

### 9.2 Produção (K8s)
- **Recomendado: CloudNativePG (CNPG)** — operator Postgres nativo K8s que entrega: WAL archiving contínuo para object storage (S3/MinIO), **backup físico** (barman), **streaming replication** (primary + replica), PITR, failover automático. Reduz drasticamente a operação manual. Compatível com Helm.
- **PITR (Point-in-Time Recovery)** via WAL archive → recuperar de "apagão acidental" até segundos antes do incidente.
- **Backup lógico complementar** (`pg_dump`) semanal — útil para restores seletivos/migrações.
- **Testar restore regularmente**: DR drill mensal em namespace efêmero. Backup não testado **não é backup** (princípio do DBA).

### 9.3 Segurança de segredos
- Senha do Postgres, chave de encriptação das `provider_keys`, secrets de providers externos: **todos via K8s Secret** (ou Vault/External Secrets Operator). Nunca em `values.yaml` do Helm, nunca em imagem, nunca em variável hardcoded.
- Conexão **TLS** ao Postgres em produção (`sslmode=verify-full`).

---

## 10. Conexões e pooling

- **Aplicação usa pool** (database/sql com `SetMaxOpenConns`, `SetMaxIdleConns`, `SetConnMaxLifetime`) — nunca abrir conexão por request.
- Para K8s com múltiplos pods: estimar `max_connections` do Postgres ≥ (pods × pool_size) + margem. 30 usuários → poucos pods → comfortable. Ainda assim, considerar **PgBouncer** (modo transaction) se o número de pods crescer, para multiplexar.
- **Long-lived connections** para o canal de batch de logs (não competir com pool de request).

---

## 11. Checklist de auto-verificação (princípios do DBA)

- [x] Modelo normalizado primeiro; sem desnormalização sem profiling.
- [x] Toda FK tem índice (explícito nos §4).
- [x] `usage_logs` particionada por mês; retention via `DROP` de partição.
- [x] Índices justificados (cada um com query e benefício declarados).
- [x] Migrações reversíveis; ferramenta escolhida (golang-migrate).
- [x] Segredos fora do repo (K8s Secret/Vault); tokens hasheados (SHA-256), provider keys encriptadas (AES-GCM com key rotation).
- [x] Concorrência tratada (UPDATE condicional, `SKIP LOCKED`, version col p/ config, debounce de `last_used_at`).
- [x] Extensibilidade: `organization_id`, `teams`, `model_aliases`, `model_pricing`, `quotas`, `cached_tokens` já no schema para absorver roadmap sem migração pesada.
- [x] Backup + PITR (CNPG) + DR drill.
- [x] Conexão com pool; arquitetura pronta para PgBouncer se escalar.
- [ ] `EXPLAIN ANALYZE` pendente — impossível agora (greenfield); adicionar como **tarefa do spec**: assim que houver tráfego real, profiling das queries críticas (validação de token, LRU, scheduler, dashboard rollup) e revisão de indexes via `pg_stat_user_indexes`.

---

## 12. Riscos e pontos de atenção para o Manager

1. **Particionamento + FK**: `usage_logs` particionada não terá FK física para `users`/`providers`/`provider_keys`/`api_tokens`. Integridade fica por conta da aplicação. **Aceitável**, mas documentar e adicionar checagens no boot (referências órfãs em job de validação periódica).
2. **Pré-criação de partições**: se faltar partição futura, **insert falha**. Mitigação: alerta de "partição para o próximo mês ausente" + automação (pg_partman/CronJob). Crítico de operação.
3. **LRU em memória**: a aplicação precisa manter cache LRU em memória (por provedor) e reconstruir do `last_used_at` no boot. Acordo com Backend Architect necessário — não é decisão pura de DB.
4. **Afinidade de cache de contexto**: também é estado **em memória** (mapa user+conversa → key), não no DB. A coluna `cached_tokens` em `usage_logs` apenas **mede** o benefício.
5. **Rate limiting em memória** vs Redis: no MVP, contador in-process perde estado em restart e não compartilha entre pods. Para 30 usuários/single-pod é tolerável; documentar como dívida técnica e migrar para Redis ao escalar.
6. **Encriptação de provider keys**: dependência de chave externa (K8s Secret). Se a chave for perdida, **todas as chaves ficam irrecuperáveis**. Mitigação: rotação documentada, backup da chave (KMS), e `encryption_key_id` para múltiplas versões coexistirem.

---

## 13. Próximos passos sugeridos (handoff ao Manager)

- Validar com **Backend Architect**: onde vive o LRU/afinidade (memória) e quem persiste `last_used_at` (app, em debounce).
- Validar com **Security Specialist**: escolha Argon2id para senhas, SHA-256 para tokens, AES-256-GCM para provider keys, gestão de `encryption_key_id`.
- Validar com **DevOps/SRE**: adoção de **CloudNativePG** no Helm chart (impacta muito a operação de backup/replica).
- Incluir no spec como **tarefa explícita**: profiling com `EXPLAIN ANALYZE` + revisão de indexes após primeiras 2 semanas de uso real.
