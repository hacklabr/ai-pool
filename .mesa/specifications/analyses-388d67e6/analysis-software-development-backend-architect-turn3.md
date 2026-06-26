# Analysis: Backend Architect — Turn 3

**Agent ID:** software-development-backend-architect
**Timestamp:** 2026-06-25T19:17:38.479Z

# Posição Final de Consenso — Backend Architect (Turno 3, posição 2/6)

Sou o **2º a falar**. Posiciono-me em relação ao Software Architect (1º) e antecipo votos nos pontos que o Manager deve arbitrar.

---

## 1. Voto 1 — Réplicas: **Opção A (1 réplica, MemoryStore, sem Redis no MVP)** ✅ CONCORDO PLENAMENTE

Como dono do núcleo do proxy, **endosso integralmente** a Opção A do Software Architect.

**Por que concordo como arquiteto do core**:
1. **Experiência de failover/afinidade consistente**: com 1 réplica, o `LRUStore`, `AffinityStore`, `RateLimiter` e estado de erro das chaves são todos consistentes — não há divergência entre pods. O usuário que recebeu afinidade para a chave K numa requisição continuará recebendo na próxima. Em multi-réplica sem Redis, isso quebra: requisição 1 vai ao pod A (afinidade → K1), requisição 2 vai ao pod B (afinidade → K2, cache perdido).
2. **Failover mais previsível**: o contador de erros por chave é único. Não há race entre pods para desabilitar a mesma chave.
3. **Rate limit correto sem esforço**: o argumento do DevOps (limite efetivo = limite × N) é eliminado por construção.
4. **Graceful shutdown no MVP** (já aceito pelo humano) cobre o caso de deploy planejado — zero downtime em RollingUpdate com `maxSurge:1`/`maxUnavailable:0`.
5. **Perda aceitável**: só perdemos HA em falha não-planejada (restart do pod em OOM/panic/crash). Para 30 usuários internos sem SLA, segundos de restart são toleráveis.

**Reserva técnica (não bloqueante)**: a transição para Opção B (Redis) deve ter **gatilho claro** documentado. Adoto os do Software Architect: SLA duro, RPS > 50/s agregado, quotas distribuídas, OU multi-tenancy real. A interface `Store` no `domain/` já deixa a troca cirúrgica (substituir 4 structs concretos).

---

## 2. Voto 2 — Cache de token: **DB hit por request (D4 do Security), SEM cache positivo no MVP**

**Posição final: voto CONTRA a proposta de cache 15s+purge do Software Architect, a favor do D4 do Security.**

Consultei o Security via `ask_peer`. Os argumentos dele convenceram-me a manter D4 sem ambiguidade:

**Argumento decisivo que muda o cálculo**: janela de 15s em 100 req/s = até **1.500 chamadas LLM** depois do disable. Para um sistema que controla custo por request, isto é **não-trivial financeiramente**, não apenas governança. Não é uma janela de "conveniência operacional" — é uma janela de exposição financeira.

**Outros argumentos do Security que endosso**:
- DB hit sub-ms com índice `UNIQUE(key_id)` é gratuito (5% de um core a 100 req/s).
- LLM call domina latência (100ms–10s). Adicionar 0.5ms é ruído.
- "Bursts de agente" é problema de **rate limit de negócio** (60/min/user), não de cache de token.
- Purge introduz 4 modos de falha reais (purge silencioso, race repopulação, multi-réplica futuro, testes não-determinísticos). D4 elimina todos.

**Concessão arquitetural**: mantenho a abstração `TokenValidator` no `domain/` (alinhado com DevOps), com `DBValidator` no MVP e `RedisValidator` futura. A porta fica aberta sem custo. Gatilho para migrar: PostgreSQL connection pool saturado, OU latência p99 > 5ms, OU escala > 1.000 usuários.

**Resposta direta ao Software Architect**: a leitura de "imediata" não precisa ser literal zero-janela — mas como DB hit entrega zero-janela **de graça**, não há motivo para aceitar qualquer janela. A complexidade do purge não se paga.

---

## 3. Voto 3 — Tabelas standalone vazias: **DEFERIR teams/quotas (concordo com Software Architect)**

Alinho com o Software Architect: **deferir `teams`, `team_members`, `quotas`, `time_window_limits`** — todas as tabelas *standalone* sem relação com `usage_logs` particionada.

**Justificativa técnica como arquiteto do core**:
- Tabelas novas são adicionadas com `CREATE TABLE` simples — **não há backfill em milhões de linhas** como ocorreria em `usage_logs` particionada.
- `teams` depende de UI de associação, lógica de ownership de chaves, RBAC granular — não existem no MVP.
- `quotas` depende de enforcement no caminho do proxy (middleware novo) — também não existe.
- Adiar não cria dívida técnica: quando o recurso chegar, é uma migration simples + feature nova.

**Mantenho no MVP (alinho com DBA e Software Architect nestes)**:
- ✅ `organizations` (seed id=1) + `organization_id BIGINT NOT NULL DEFAULT 1` em todas as tabelas multitenant (prepara multi-tenancy sem custo).
- ✅ `model_aliases`, `model_pricing` (nullable) — pavimentam aliasing e cost tracking sem custo.
- ✅ `cached_tokens` e `cost_usd` em `usage_logs` (colunas nullable) — capturam métricas do diferencial do produto desde já.
- ✅ `encryption_key_id` em `provider_keys` — pavimenta rotação de KEK.
- ✅ `context_window` em `models` — pavimenta failover cross-provider.
- ✅ `audit_log` append-only.

**Princípio consolidado**: adicionar AGORA só colunas/relações que tocam `usage_logs` particionada ou `provider_keys` (alto churn). Tabelas standalone podem esperar.

---

## 4. Pontos técnicos do core — consolidação final

Como dono do núcleo do proxy, fixo estas decisões arquiteturais como **consolidadas**:

### 4.1 Streaming SSE com flush
- Handler HTTP puro (não Controller Beego), conforme ADR-007.
- Escrever direto no `http.ResponseWriter` subjacente + `http.Flusher.Flush()` por chunk.
- Headers antes do primeiro Write: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`.
- 3 timeouts via `context`: first-byte (30s), idle entre chunks (60s), overall cap (5–10min).
- Cancelamento do cliente propagado ao upstream via `r.Context().Done()` (poupando tokens/custo).
- `bufio.Reader.ReadString('\n')` (não Scanner default que estoura buffer em chunks grandes).
- Header upstream: `Accept-Encoding` removido (streamar texto puro, simplifica debugging).

### 4.2 Cadeia de failover — orquestrador STATEFUL externo ao pipeline
**Resposta direta ao Software Architect sobre ADR-002**: FailoverPolicy NÃO é uma policy dentro do pipeline. É **orquestrador stateful externo que invoca o pipeline N vezes**.

Justificativa técnica:
- Pipeline de políticas (HealthFilter → AffinityPolicy → LRUPolicy) é **stateless por requisição** — escolhe UMA chave candidato.
- Failover mantém estado entre tentativas: chaves já tentadas, contagem de erros, decisão de parar.
- Modelar failover como policy quebraria a semântica stateless do pipeline.

Design consolidado:
```
FailoverChain (stateful: tried_keys[], attempt_count)
  └─ loop until success or exhausted:
       ├─ candidate = pipeline.Execute(request, exclude=tried_keys)
       ├─ if candidate == nil → break (no more keys)
       ├─ response = forwarder.Send(candidate)
       ├─ if success → return response
       ├─ else:
       │    ├─ classifier.Classify(error) → action
       │    ├─ keymgmt.RecordError(candidate, action)
       │    ├─ tried_keys.add(candidate)
       │    └─ backoff.Wait()
       └─ return final error to client
```

**Classificação de erros** (decide retry vs abort):
- Retry: 401/403, 408, 429, 5xx, network/timeout.
- Abort (não retry): 400, 404, 413, 422.
- Pós-primeiro-chunk-usável em streaming: erro é terminal (sem failover transparente).

**Backoff**: curto (100ms inicial, ×2, cap 1–2s, full jitter) porque é entre chaves distintas. Respeitar `Retry-After` se ≤ threshold.

### 4.3 Afinidade de cache de contexto — X-Conversation-ID + hash fallback, em memória
- **Primário**: cliente envia `X-Conversation-ID` → lookup direto em `AffinityStore[user_id, conv_id]`.
- **Fallback heurístico**: hash SHA-256 (128 bits truncado) do prefixo normalizado das últimas K=20 mensagens.
- Continuidade = `n > k` E `M_prev` é prefixo estrito de `M_curr` (após normalização: minify JSON, ordenar keys, considerar só `role`+`content`+`tool_calls`).
- **Store em memória com TTL 30min**, LRU eviction (50 conversas/usuário). Por usuário → sem colisão cross-user.
- Se chave em afinidade está em cooldown → cai para LRU (best-effort).
- Alinho com Security: **NÃO persistir hash de conteúdo em DB** (LGPD). `cached_tokens` em `usage_logs` apenas mede o benefício.

### 4.4 Auto-disable com parser registry
- Contador de erros consecutivos por chave, threshold configurável (default 5).
- Estados: `enabled` / `auto_disabled` (com `disabled_until`) / `disabled_manual`.
- `ErrorParser` interface no `domain/`, registry `map[provider_id]Parser`, fallback genérico com regex (ISO 8601, Unix epoch, "in N seconds").
- Parser retorna `{DisableUntil, IsFatal, Reason}`.
- Reativação: `disabled_until` em PostgreSQL + scheduler (ticker 30s) + lazy reativação opcional.
- Backoff crescente quando parser não encontra data (5min → 30min → 2h → 24h).

### 4.5 Rate limiting — token bucket abstraído
- `RateLimiter` interface no `domain/`, `MemoryLimiter` no MVP (token bucket por usuário, com `golang.org/x/time/rate`).
- Configurável por usuário (admin). Default: 60 req/min com burst 10.
- Resposta 429 + `Retry-After`. Body NÃO revela limite exato (info disclosure).

### 4.6 Registro de uso assíncrono
- Canal bufferizado `chan UsageRecord` (capacidade 1000).
- **Non-blocking send** com `select` + default → se cheio, logar stderr + descartar (preferível a bloquear cliente).
- Worker goroutine: batch insert (50 records OU 500ms) em `usage_logs`.
- Captura de tokens: `response.usage` não-stream; em stream, último chunk se `stream_options.include_usage: true` (inserir essa opção no request upstream se cliente não enviou).
- Latência medida do recebimento ao último byte enviado ao cliente.
- `failed_keys` (array de key_ids tentadas) registrado para diagnóstico.

---

## 5. Confirmação do plano de execução de alto nível

**Adoto integralmente as 11 fases do Software Architect (Turno 1, §8)**, com destaques para o core:

| Fase | Foco | Dono / Observação |
|---|---|---|
| 0 — Fundações | Scaffold, docker-compose, migrations base, config, `/healthz`, `/readyz` | Todos |
| 1 — Domínio & Persistência | Entidades, repositórios pgx, envelope encryption | Backend + DBA |
| 2 — Identidade & RBAC | argon2id, tokens HMAC+pepper, sessão PostgreSQL | Backend + Security |
| 3 — **Proxy Core** ← validação crítica | Handler HTTP puro, streaming SSE, HTTP client por provider, forward para 1 chave fixa | **EU (Backend)** — marcos: `curl -N` streaming funciona |
| 4 — **Routing Engine** | HealthFilter, LRU, FailoverPolicy (orquestrador externo), estado de chave in-memory | **EU (Backend)** |
| 5 — Health Engine & Parsers | State machine, ErrorParser registry + fallback, scheduler de reativação, toggle admin | **EU (Backend)** |
| 6 — **Afinidade de Cache** ← diferencial | X-Conversation-ID + hash fallback, TTL 30min, métricas hit/miss | **EU (Backend)** |
| 7 — Rate Limiting | MemoryLimiter (token bucket) | Backend |
| 8 — Observabilidade de Uso | Logger assíncrono, batch insert, endpoints dashboard | Backend + DBA |
| 9 — Web UI | Beego templates + HTMX + Chart.js, server-rendered | Frontend |
| 10 — Hardening & Deploy | Graceful shutdown, Helm chart, Prometheus stub, auditoria, testes carga | DevOps + Security |

**Marcos de demonstração** (alinho com Software Architect):
- Pós-Fase 3: cliente OpenAI SDK funciona via proxy com streaming.
- Pós-Fase 5: failover e auto-reativação demonstráveis.
- Pós-Fase 9: MVP usável no dia a dia (critério de sucesso do briefing).

**Dependência crítica entre fases**: Fase 6 (Afinidade) depende de Fase 4 (Routing Engine com pipeline). Implementar Afinidade como política que insere no pipeline ANTES do LRU.

---

## 6. Tabela de votos consolidada para o Manager

| Ponto | Voto Backend Architect |
|---|---|
| **(1) Réplicas** | **Opção A** (1 réplica, MemoryStore, sem Redis no MVP). Convergência Backend + Software Architect. |
| **(2) Cache de token** | **DB hit por request (D4)**. Voto contra cache 15s+purge do Software Architect. Alinho com Security. |
| **(3) Tabelas standalone vazias** | **Deferir** teams/quotas/time_window_limits. Alinho com Software Architect. |

---

## 7. Consensos já consolidados (não reabrir)

Confirmo e endosso todos os pontos já acordados:
- Hexagonal architecture + pgx v5 + pgxpool + golang-migrate.
- Beego Controller com bypass no hot path (ADR-007).
- `organization_id BIGINT NOT NULL DEFAULT 1` em todas as tabelas multitenant.
- HMAC-SHA256 com pepper externo para tokens `sk-aipp-...`.
- ESO para KEK; envelope encryption (DEK por provider_key + KEK externo).
- HTMX não SPA; sessão server-side + CSRF para UI; Bearer para API do proxy.
- `usage_logs` particionada por mês com `DROP` para retention.
- `audit_log` append-only via grants PostgreSQL.
- Graceful shutdown no MVP (60s grace + preStop sleep 10s).
- `slog` para logging estruturado.
- Store abstrações (`LRUStore`, `AffinityStore`, `RateLimiter`, `TokenValidator`) no `domain/` com `MemoryStore` concreta.

---

## 8. Self-check final

1. **Defendi o que era meu defender?** Sim — todas as 6 áreas do core (streaming, failover, afinidade, auto-disable, rate limit, registro de uso) estão fixadas com design específico.
2. **Cedi onde o argumento era forte?** Sim — cedi em graceful shutdown (puxado do ROADMAP), em store abstrações (interfaces sim, Redis não), em organization_id NOT NULL DEFAULT 1 (vs nullable).
3. **Mantive onde era tecnicamente correto?** Sim — DB hit sem cache positivo (contra Software Architect, a favor de Security), failover como orquestrador stateful externo ao pipeline (esclareço ADR-002).
4. **Há divergência irreconciliável?** Apenas com o Software Architect em cache de token (ponto 2). Deixo para o Manager arbitrar — minha posição é firme mas respeito o voto divergente.
