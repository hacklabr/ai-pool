# PRD — AI Provider Pool (documento vivo)

> Criado em: 2026-08-13 · Última revisão: 2026-08-13
> Regra: doc desatualizado é corrigido ou marcado como obsoleto — nunca deixado apodrecendo em silêncio.
> Documento vivo de referência: descreve **como o produto é hoje**, derivado do código.
> Fontes: `README.md`, `DECISIONS.md`, `ROADMAP.md`, `internal/`, `web/`, `migrations/`.

## 1. Propósito e público

O **AI Provider Pool** é um **proxy/load-balancer de LLMs compatível com a API
OpenAI**. Em vez de cada pessoa usar sua própria chave de provedor (ou
compartilhar chaves por mensageiro), todos os clientes OpenAI-compatible apontam
para **um único endpoint**. O pool balanceia carga entre chaves, faz failover
transparente, aproveita o cache de contexto dos provedores (economia real de
tokens) e governa quem usa o quê.

**Público:** ~30 usuários internos (Hacklab), sem SLA duro. Dois papéis:
`admin` (gestão completa) e `user` (autoatendimento de tokens e visão do próprio uso).

**Stack:** Go + PostgreSQL 16 · proxy em `net/http` puro (SSE via `http.Flusher`) ·
admin server-rendered (`html/template` + HTMX + Alpine + ECharts) · pgx v5 ·
golang-migrate · Helm chart com PostgreSQL embutido · imagem distroless nonroot.

## 2. Requisitos funcionais

### RF-01 — Pool de provedores, modelos e chaves com failover
- CRUD de **provedores** (nome, slug único por org, `base_url` com SSRF guard,
  `api_spec` ["openai" default], override global de User-Agent, ativo/inativo).
- CRUD de **modelos** por provedor (nome real, display name, `context_window`,
  `output_limit`, modalidades ["text","image"], ativo/inativo).
- CRUD de **chaves** por provedor (rótulo + valor cifrado na escrita; valor jamais
  retorna pela UI), com override de User-Agent por chave.
- **Roteamento por request:** modelo → provedor ativo → chave, selecionando:
  1. **Afinidade de cache de contexto** — header `X-Session-Id`/`X-Conversation-ID`
     preferido; fallback hash SHA-256 do prefixo normalizado das mensagens
     (hash nunca logado/persistido). TTL 30 min, máx. 50 entradas/usuário.
  2. **LRU** (least-recently-used) entre chaves habilitadas.
- **Failover transparente** entre chaves do mesmo provedor em 401/403/429/5xx/
  erro de rede/timeout; **não** faz failover em 400/404/413 nem após o primeiro
  chunk útil do stream. Backoff 200 ms–5 s com full jitter (`crypto/rand`).
- Resposta inclui header `X-Context-Cache: HIT|MISS`.

### RF-02 — Proxy OpenAI-compatible
- Endpoints `/v1/*` e `/api/*` com Bearer `sk-aipp-...`; body original preservado
  byte a byte (sem re-serialização); streaming SSE com 3 timeouts (first-byte,
  idle-chunk, overall — configuráveis; **defaults atuais no código: 5 min / 3 min /
  10 min** — o registro D1 do DECISIONS.md está desatualizado, ver doc-bugs).
- Erros do provedor são **repassados no formato original** (ex.: 429 dispara
  retry do SDK OpenAI); erros do proxy seguem o formato OpenAI
  (401 token, 400 body, 413 payload, 404 modelo, 503/502 indisponibilidade).
- Limite de body: 10 MB (configurável).

### RF-03 — Auto-disable e reativação de chaves
- **5 erros em janela de 2 min** → chave vai para `auto_disabled` (threshold/janela
  configuráveis), com motivo classificado (`quota_exceeded`, `rate_limited`,
  `error_threshold`, `auth_failed`, `parse_failed_retry`).
- **Parse da mensagem de erro do provedor** (registry de parsers por provedor)
  para extrair a data de liberação de cota → `disabled_until` preciso.
- Se o parse falha: backoff crescente 1m/5m/15m/1h/6h/24h por ciclo consecutivo.
- **Reativação dupla:** scheduler a cada 30 s (`FOR UPDATE SKIP LOCKED`) +
  reativação lazy no próximo request; LRU store notificado.
- Admin pode **force-disable/force-enable** (`manual_disabled` ignora `disabled_until`).
- **Teste de chave sob demanda** (`POST /keys/{id}/test` e `/keys/test-all`):
  chamada real `chat/completions` com `max_tokens=1` e sem `temperature`
  (provedor aplica o default); 200 reabilita, 401 marca `auth_failed`; 403
  marca `auth_failed` EXCETO quando o corpo indica limite concorrente
  transitório (ex.: "concurrent request limit") — não altera o status, apenas
  reporta; 429 marca `quota_exceeded` com `Retry-After`.

### RF-04 — Tokens de API para usuários (`sk-aipp-...`)
- Formato `sk-aipp-<key_id 8>-<secret 48>`; valor completo exibido **uma única vez**.
- Persistido apenas `key_id` (lookup O(1)) + `token_hmac` (HMAC-SHA256 com pepper
  externo); revogação soft (`revoked_at`), expiração opcional, `last_used_at`.
- Usuário lista/cria/revoga **seus** tokens (anti-IDOR por escopo de sessão);
  admin lista/revoga/exclui tokens de qualquer usuário.
- Validação com **DB hit por request** (revogação instantânea); usuário
  desabilitado perde acesso imediatamente.

### RF-05 — Painel admin (server-rendered)
- Telas: login (multi-step com slots MFA/OAuth reservados), dashboard, usuários,
  detalhe de usuário, provedores, modelos, chaves (4 estados visuais +
  force enable/disable), tokens, meus tokens, meu uso, auditoria.
- RBAC `admin`/`user`: `/web/admin/*` e `/api/web/admin/*` exigem role admin;
  `/web/me/*` e `/web/dashboard` para qualquer role (user vê só o próprio uso).
- Sessão cookie `aipp_sess` HttpOnly + SameSite=Lax + Secure (prod), TTL 12 h;
  CSRF double-submit (`aipp_csrf` + header `X-CSRF-Token`).
- Login com rate limit por IP e **timing equalization** (hash Argon2id
  fake para usuário inexistente — anti-enumeração).
- Admin **não pode desabilitar a si mesmo** (anti-lockout).

### RF-06 — Convites com setup automático de cliente
- Admin cria convite informando só o username; recebe **comando de setup único**:
  `curl -fsSL <url>/invite/<token> | bash`.
- Token `inv-<prefixo 8>-<segredo 32B>`; DB guarda só SHA-256; uso único;
  expiração 24 h; revogável/excluível.
- `GET /invite/{token}` retorna o **mesmo script** para token válido/inválido/
  usado/expirado (anti-enumeração); `POST .../accept` rate-limitado (5/15 min/IP).
- Aceite (transação `SELECT ... FOR UPDATE`): cria usuário (senha Argon2id,
  mín. 12 chars) + token `sk-aipp-...` (nome "opencode") e retorna catálogo de
  modelos ativos (com modalidades e bloco `limit` context/output no schema OpenCode).
- O script configura automaticamente **OpenCode** (`~/.config/opencode/opencode.json`)
  e/ou **MiMo-Code** (`~/.config/mimocode/mimocode.json`) com o provider `llm-pool`.

### RF-07 — Registro de uso e dashboard
- **Coletor assíncrono não-bloqueante:** canal de 10.000 eventos; flush a cada
  100 eventos ou 1 s; descarta com métrica de drop se cheio; **dead-letter table**
  em falha de insert (reprocessável).
- `usage_logs` **particionada mensal** (tokens prompt/completion/cached/total,
  latência, status, error_class, IP, UA; `cost_usd` nullable pavimentado).
- **Rollup job** agrega `usage_logs` → `usage_rollups` (bucket horário,
  upsert idempotente, execução a cada 1 min no código atual) para dashboard em ms.
- Dashboard admin: KPIs (requests, tokens por tipo, **cached_tokens** como KPI de
  economia, erros) + 3 gráficos ECharts, janela from/to (default 7d), group by
  user|provider|model — consulta somente rollups. Usuário comum vê "meu uso".

### RF-08 — Rate limiting
- Token bucket in-memory; **60 req/min por token** (burst 10; configurável);
  429 + `Retry-After`; hit de limite registrado no usage log (error_class
  `rate_limit`).
- Login admin: rate limit por IP (10/min configurado, ver doc-bug sobre o escopo
  não estar efetivo); aceite de convite: 5/15 min por IP.
- Tabela `rate_limits` persiste **configuração** por escopo (global/org/user/token);
  a contagem fica fora do DB (sem write amplification no hot path).

### RF-09 — Auditoria
- `audit_log` **append-only** por convenção de aplicação (só `Append`/`List` no
  repositório; enforcement por GRANTs de banco é responsabilidade de IaC, não
  aplicado na migration); tela admin com filtros e JSON paginado.
- **Cobertura atual:** apenas ações de convites gravam auditoria. CRUDs de
  usuários/provedores/chaves/modelos/tokens ainda não auditam (dívida conhecida).

### RF-10 — Saúde e operação
- `/healthz` (liveness) e `/readyz` (hoje sempre 200 — não checa dependências);
  graceful shutdown (drena streams; deploys RollingUpdate zero-downtime);
  `/metrics` Prometheus **gateado** por `ENABLE_METRICS` (instrumentação sempre
  ativa — 6 métricas).
- Bootstrap de admin via CLI one-shot idempotente (`aipp --bootstrap-admin-...`,
  senha ≥ 12 chars); migrations via `aipp --migrate-only` (Helm post-install Job).

### RF-11 — Sincronização automática do catálogo de modelos
- Job periódico in-process (pacote `internal/modelsync/`, padrão RollupJob)
  consulta `GET {base_url}/models` de cada provedor ativo (1 chave usável,
  Bearer decriptada, timeout ~30 s por provedor) e reconcilia a tabela `models`.
- Reconciliação **fail-safe** (ADR-0015): provedor OK → adiciona novos (ativos,
  só com nome), reativa os que retornaram, **desativa** os ausentes; provedor
  indisponível/sem chave/resposta inválida → **nenhuma alteração**.
- **Nunca deleta** linhas; **nunca sobrescreve** enriquecimento manual
  (`context_window`, `output_limit`, `modalities`, `model_pricing`,
  `model_aliases`) de modelos existentes.
- CLI idempotente `aipp --sync-models` (padrão `--migrate-only`) roda uma
  rodada e sai: exit 0 mesmo com provedores falhando (fail-safe); exit 1 só em
  falha estrutural (DB indisponível, KEK ilegível). Executado pelo Helm Job
  após as migrations — instalação nasce com o catálogo populado.
- Intervalo via env `AIPP_MODEL_SYNC_INTERVAL` (Go duration, default 6h;
  `0` desativa o job periódico). Sync inicial assíncrono no boot (fail-open,
  nunca bloqueia readiness).
- **Catálogo público de especificações** (ADR-0017, `internal/modelsdev/`):
  fetch de `https://models.dev/api.json` (mesma fonte que o OpenCode usa)
  1x por rodada de sync, com `context_window`/`output_limit`/`modalities`/
  `reasoning_options` por model-id — lookup global com prioridade para
  provedores de primeira parte (zai/kimi/moonshotai). Modelo novo presente
  no catálogo nasce já configurado; existente com campo vazio recebe
  backfill (`ModelRepo.BackfillSpecs`). Fetch indisponível → sync segue sem
  enriquecimento (fail-safe; próximo tick tenta de novo).
- **Variações de reasoning effort** (ADR-0017): `reasoning_options`
  persistido em `models.reasoning_options jsonb` (ex.: `["low","high","max"]`)
  e exposto ao OpenCode como `variants` com `reasoningEffort` — o cliente
  envia como parâmetro `reasoning_effort` do request; o proxy permanece
  byte-passthrough (sem IDs sintéticos).
- A invariante ADR-0015 vale também para o dicionário: **só preenche campo
  vazio (nulo), nunca sobrescreve** (garantida no próprio SQL via
  `COALESCE`/`CASE`). O relatório de sync contabiliza `enriched` por provedor
  (log, CLI, resumo do botão admin).

### RF-12 — Listagem agregada de modelos
- `GET /v1/models` e `GET /api/models` autenticados pelo mesmo middleware do
  proxy (Bearer `sk-aipp-...`; sem token → 401), respondendo a partir do
  **catálogo local** (modelos ativos de provedores ativos da organização).
- Formato OpenAI: `{"object":"list","data":[{"id","object":"model","created",
  "owned_by":<provider slug>}]}`, com dedup por nome (primeiro provedor ativo
  vence — mesma precedência do hot path).
- Variante `?format=opencode`: mapa no schema OpenCode
  (`provider.models.{id} = {name, modalities?, limit?}`), mesmo formato do
  aceite de convite (lógica compartilhada em
  `internal/adapters/httpserver/modelcatalog`).

### RF-13 — Script de atualização do cliente OpenCode
- Rota pública `GET /setup/update-models.sh` (admin mux, junto a `/invite`)
  serve script bash (`curl | bash`) com `set -euo pipefail`, exigindo `jq`.
- O script lê `$OPENCODE_CONFIG` ou `~/.config/opencode/opencode.json`,
  extrai `apiKey`/`baseURL` do provider `llm-pool` (ausentes → mensagem
  amigável instruindo rodar o instalador), faz backup
  (`opencode.json.bak-<timestamp>`), chama `GET {baseURL}/models?format=opencode`
  e reescreve **apenas** `provider["llm-pool"].models` via jq, preservando
  todo o restante da configuração. Nunca ecoa o token.

## 3. Requisitos não-funcionais

### RNF-S — Segurança
- **Envelope encryption AES-256-GCM** das chaves de provedores
  (`nonce(12) || ciphertext || tag`); KEK de 32 bytes via **arquivo readOnly**
  (Secret/ESO), nunca env var nem DB; `encryption_key_id` por chave suporta
  **rotação gradual de KEK** (múltiplas KEKs registradas).
- Tokens: HMAC-SHA256 + pepper externo; convites: SHA-256; senhas: Argon2id.
- **SSRF guard** na `base_url` de provedores (bloqueia loopback, RFC1918,
  link-local, metadata cloud 169.254.x; https obrigatório fora de dev).
- Logs JSON estruturados **sanitizados** (`SanitizingLogger`); segredos decifrados
  zeroizados após uso e jamais logados; hash de afinidade jamais persistido.
- UI: `html/template` (escape automático), JS via CDN com **SRI**, cookies
  HttpOnly/SameSite, CSRF double-submit, tokens mascarados (`sk-aipp-AB12CD34-•••`).
- DB least-privilege recomendado (role app sem UPDATE/DELETE em `audit_log`);
  DEV_MODE sem KEK usa `IdentityDecryptor` com WARN loud.

### RNF-P — Performance
- 1 réplica, stores in-memory (`LRUStore`, `AffinityStore`, `RateLimiter`,
  `SessionStore`) — decisão consciente (ADR-0001) para 30 usuários; interfaces
  prontas para Redis futuro.
- Validação de token: lookup O(1) por `key_id` (índice parcial); dashboard lê só
  rollups; persistência de uso fora do hot path; body limite 10 MB; streaming
  sem buffering de framework.

### RNF-O — Observabilidade
- Logs JSON com níveis; logs CONTEXT CACHE HIT/MISS; 6 métricas Prometheus no
  código (endpoint gated); preparado para OpenTelemetry (request_id/trace_id);
  stack recomendada Loki + Fluent-bit + Grafana.

### RNF-R — Confiabilidade e retenção
- Graceful shutdown; dead-letter de usage; partição `default` de `usage_logs`
  como alarme de partição faltante; **retenção planejada: DROP de partição de
  `usage_logs` aos 90 dias — NÃO implementada** (ver doc-bugs/pendências);
  rollups por anos, auditoria sem retenção automática.
- Lock otimista (`version`) em config admin; transições de chave via CAS atômico.

### RNF-D — Deploy/compatibilidade
- Helm: app + PostgreSQL embutido (`postgres:16-alpine`, senha auto-gerada
  persistida, PVC com `resource-policy: keep`) ou DB gerenciado via Secret;
  migration Job post-install; NetworkPolicy opcional; distroless nonroot;
  suporte a imagem em registry privado (pull secret).
- Compatibilidade com SDKs OpenAI e coding tools (OpenCode/MiMo) via `/v1` e `/api`.

## 4. Critérios de aceite gerais
1. Cliente OpenAI-compatible troca apenas `baseURL` + `apiKey` e funciona
   (incluindo streaming), sem mudança de código.
2. Com N ≥ 2 chaves habilitadas, a falha de uma é invisível ao cliente
   (failover antes do primeiro chunk) e a chave ruim se auto-desabilita e
   reativa sozinha.
3. Chaves de provedores nunca aparecem em logs, UI ou DB em plaintext; dump do
   DB não compromete chaves nem tokens.
4. Token revogado ou usuário desabilitado perde acesso no request seguinte.
5. Dashboard responde em ms sobre rollups e mostra KPI de `cached_tokens`.
6. Deploy Helm único sobe app + banco + migrations, com zero-downtime em upgrade.
7. `go test -race ./...` e pipeline CI (vet, gofmt, gosec, govulncheck, trivy
   HIGH/CRITICAL, helm lint) verdes.

## 5. Fora de escopo atual (adiado pelo ROADMAP)
- Multi-réplica + Redis (interfaces prontas), HPA/PDB, sticky sessions.
- Ativação do `/metrics` + ServiceMonitor (instrumentação já existe).
- CronJob de pré-criação de partições mensais de `usage_logs` e retenção 90d
  (pendência crítica — partições pré-criadas só até 2026-09).
- Resolução de **model aliasing** (campo `model_aliases` existe) e modelo "auto".
- Health check periódico de chaves em background (hoje: teste manual na UI).
- Times, quotas de tokens por usuário/time, limitação de horário.
- Multi-tenancy real (`organization_id` pavimentado em todas as tabelas).
- Login OAuth (slot reservado no login multi-step).
- Notificação/webhook quando chave é desabilitada.
- **Cálculo de custo** por provedor/modelo e dashboard de custos
  (`model_pricing` e `usage_logs.cost_usd` existem, nullable).
- Failover cross-provider (risco de truncamento por context windows diferentes).
- mTLS interno; **rotação de KEK automatizada** (procedimento manual documentado
  em `internal/adapters/crypto/envelope.go`); Cosign + SLSA (placeholder no CI).
- Expiração de token configurável na UI (campo existe, form não expõe).
