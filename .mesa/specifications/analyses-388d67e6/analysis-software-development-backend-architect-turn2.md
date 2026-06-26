# Analysis: Backend Architect — Turn 2

**Agent ID:** software-development-backend-architect
**Timestamp:** 2026-06-25T18:55:26.760Z

# Delta — Backend Architect (Turno 2)

Foco: tensões com pares, pontos de convergência e divergência, revisões da minha análise de Turno 1.

---

## 1. Beego isolado (vs Software Architect) — concordo, com ressalva de severidade

ADR-007 do Software Architect (Proxy Core como handler HTTP puro fora do Controller Beego; Beego só no admin/UI) é o ponto de maior convergência entre nós. **Adoto integralmente.**

**Ressalva técnica (divergência menor)**: o argumento dele de que "Beego bufferiza resposta" é tecnicamente correto mas a severidade é levemente exagerada. Beego v2 NÃO força buffering por padrão — Controllers são wrappers sobre `http.ResponseWriter` que expõem `http.Flusher` normalmente. O risco REAL é usar `Controller.ServeJSON` ou APIs de conveniência que escrevem em buffer interno antes do flush. Se escrevermos direto no `http.ResponseWriter` subjacente com `http.Flusher.Flush()`, streaming SSE funciona com Beego.

**Mesmo assim, o design dele é mais defensável** porque:
1. Cria limite limpo entre "transporte admin MVC" e "caminho quente HTTP puro" — facilita migração futura.
2. Reduz acoplamento do hot path ao framework.
3. Custo adicional desprezível.

**Rejeição parcial**: não endosso a implicação de que Gin/Echo seriam "mais naturais". Para `net/http` puro com Flusher, o framework é irrelevante no hot path — a escolha Beego só impacta admin/UI. Aceitamos Beego sem remorso.

---

## 2. Store abstraction (vs DevOps) — convergência parcial, posição revisada

DevOps levanta o **ponto mais forte de todo este round**: rate limit em multi-réplica quebra sem estado compartilhado (limite efetivo = limite × N). **Eu subestimei isso no Turno 1.**

**Posição revisada**:
- ✅ **No MVP**: interfaces `LRUStore`, `AffinityStore`, `RateLimiter`, `CircuitBreakerStore` no `domain/`.
- ✅ **No MVP**: implementação única `MemoryStore` (in-memory + mutex).
- ✅ **No MVP**: `replicas: 1` em prod inicial (aceitando downtime < 30s em restart).
- ❌ **Fora do MVP**: implementação `RedisStore`. Gatilho para introduzir: `replicas > 1` em prod, OU chegada de ROADMAP de teams/quotas distribuídas, OU volume real de QPS justificar.

**Justificativa para adiar Redis**:
- 30 usuários internos + 1 réplica = uptime alcançável sem Redis.
- Adicionar Redis = +1 serviço para operar, monitorar, fazer backup, recuperar. Para equipe pequena, custo operacional não trivial.
- Interfaces bem desenhadas tornam a migração cirúrgica (substituir 4 structs concretos).
- O argumento "rate limit efetivo = limite × N" só é problemático quando N > 1. Mantendo N=1, é irrelevante.

**Open question para DevOps no consenso**: aceita `replicas: 1` em prod no MVP com SLA best-effort, ou insiste em Redis já no MVP? Para mim, este é o **ponto de maior incerteza** do round.

---

## 3. Graceful shutdown (vs DevOps) — concordo em puxar para MVP

DevOps tem razão: sem graceful shutdown, deploys RollingUpdate cortam streams de chat visivelmente. **Apoio puxar do ROADMAP para o MVP.**

**Impacto específico no streaming SSE (meu domínio)**:

Implementação proposta:
1. SIGTERM/SIGINT → `shutdownCtx, cancel := context.WithTimeout(bgCtx, 60s)`.
2. `http.Server.Shutdown(shutdownCtx)` (Beego v2 expõe).
3. Comportamento esperado:
   - **Novas conexões**: recusadas imediatamente (readiness probe falha → Ingress para de rotear).
   - **Conexões em curso (incluindo SSE)**: recebem `shutdownCtx.Done()`. O forwarder propaga `ctx.Err()` ao upstream → cancela request → goroutines de stream finalizam → poupamos tokens/custo no provedor.
4. **Problema específico de SSE**: requisições podem durar minutos. Cortar com 60s de grace aborta chat em andamento. **Mitigação**:
   - `preStop` hook no K8s com `sleep 10` antes do SIGTERM (dá tempo do Ingress parar de mandar novas conexões).
   - `terminationGracePeriodSeconds: 60` (alinhado com DevOps).
   - **Decisão de produto aceitável**: streams em curso podem ser cortados a cada deploy. Para 30 usuários internos com deploys infrequentes, é aceitável. Documentar.

**Sinergia com meu design de Turno 1**: já havia desenhado propagação rigorosa de `r.Context().Done()` ao upstream. Com graceful shutdown, basta o `shutdownCtx` ser pai de todos os contextos de request ativos — nenhuma mudança arquitetural, apenas amarrar o contexto raiz.

---

## 4. Token caching (vs Security) — confirmo DB hit por request

Security propõe D4 (MVP sem cache positivo de token, DB hit por request). **Confirmo e defendo.**

**Por que não introduzir cache positivo mesmo assim**:
- A query (`SELECT ... FROM api_tokens WHERE key_id = ? AND status='active' AND (expires_at IS NULL OR expires_at > now())`) em índice `UNIQUE(key_id)` tem latência sub-milissegundo.
- Para 30 usuários em uso interativo (chat), QPS máximo realista ~50 req/s agregadas. PostgreSQL aguenta 10k+ QPS sem suar.
- **Revogação imediata garantida**: zero janela de cache. Atende requisito explícito do briefing.
- Cache positivo só faria sentido se houvesse >>1k req/s, o que é irreal neste escopo.

**Resolução de tensão Security vs DBA sobre hashing de token**:
- Security: `HMAC-SHA256(secret, pepper)` (pepper externo via KMS/Secret).
- DBA: `SHA-256(puro)` (argumenta que o token já tem entropia suficiente, rainbow tables inviáveis).
- **Minha posição: HMAC com pepper (Security)**. Razão: defesa em profundidade. Se o DB vaza E o atacante tem GPU farm, SHA-256 puro permite rainbow-tablear tokens mesmo de alta entropia — lento mas viável em escala. Com HMAC + pepper externo, mesmo com DB + algoritmo conhecido, fica inviável sem o pepper. Custo no request path: um HMAC-SHA256 (microssegundos). Vale.

---

## 5. Migration + DB layer (vs DBA/DevOps) — alinhamento consolidado

DBA recomenda **golang-migrate**, DevOps menciona `pgxpool`. **Alinho e detalho**:

**Escolha: `pgx` v5 direto + `pgxpool` + golang-migrate**.

Justificativa técnica:
- `pgx` v5 é o driver PostgreSQL nativo mais performático em Go. Suporta binary protocol, prepared statements cacheados, `COPY`, tipos nativos (INET, JSONB, CITEXT, TIMESTAMPTZ) sem boilerplate.
- `database/sql` é abstração genérica que adiciona overhead e perde features.
- `pgxpool` é o pool nativo do `pgx`.

**Alternativa considerada — sqlc** (gera código Go type-safe a partir de SQL): type-safety total, sem reflection. Mas exige workflow adicional (`sqlc generate`) e curva de adoção. **Posiciono como MVP+1**, não MVP. Para este volume, `pgx` direto com queries em métodos de repositórios é suficiente.

**ORM (GORM/Beego ORM) — rejeitado para o hot path**:
- Beego ORM é acoplado ao framework (Software Architect concorda).
- GORM adiciona "magic" e complexidade desnecessária para o volume.

**Recomendação consolidada**:
- Migrations: `golang-migrate` com `.up.sql`/`.down.sql` versionados no git, roda via Helm hook Job (multi-réplica) ou init-container (single-réplica).
- Queries: `pgx` v5 direto em repositórios (`internal/repository/`).
- Pool: `pgxpool`, max conns ~10 (dev) / ~25 (prod).
- DDL disciplinado (DBA §8.2): migrations reversíveis, `CREATE INDEX CONCURRENTLY`, `ADD COLUMN` em 2 passos para tabelas grandes.

**Sinergia com hexagonal architecture**: repositórios `pgx` implementam interfaces de domínio definidas em `domain/`. Trocar para outro driver é localizado. Mantém domínio puro.

---

## 6. Schema do DBA — aceito com 2 ajustes

Schema do DBA é sólido. Aceito a maioria. **Dois ajustes**:

### Ajuste 1: `organization_id NOT NULL DEFAULT 1` (alinho com DBA)

Tensão: DBA recomenda `NOT NULL DEFAULT 1`; Software Architect oferece nullable como Opção B; Security prefere nullable.

**Posiciono-me com DBA (NOT NULL DEFAULT 1)**:
- Força consistência: queries sempre podem (e devem) filtrar por `organization_id`.
- Nullable cria ambiguidade (o que significa `organization_id IS NULL`?).
- DEFAULT 1 elimina esforço cognitivo. Custo: zero.
- O risco de "leakage entre tenants futuros" mencionado pelo Security só é problemático quando há múltiplos tenants — no MVP há só a default. E quando multi-tenancy chegar, o `NOT NULL` força todo mundo a pensar em tenant desde o início (disciplina positiva).

### Ajuste 2: granularidade do schema

DBA propõe ~12 entidades, incluindo `teams`, `model_aliases`, `model_pricing`, `quotas`, `time_window_limits` vazias.

**Posicionamento granular**:
- ✅ Criar agora (custo ~zero, evita migração cara): `organizations` (seed id=1), `model_aliases`, `model_pricing`, `quotas`, `cached_tokens` e `cost_usd` em `usage_logs`, `encryption_key_id` em `provider_keys`, `context_window` em `models`, `audit_log`.
- 🟡 Adiar (divergência menor com DBA): `teams` + `team_members`. Razão: dependem de UI e lógica de associação que não existem no MVP. Tabela nova é fácil criar depois (não é backfill em milhões de linhas). **Se o DBA insistir que é "quase grátis", aceito criar vazias.** Não é meu hill to die on.
- ✅ `disabled_reason` CHECK com enum (`manual`, `quota_exceeded`, `rate_limited`, `error_threshold`, `auth_failed`, `parse_failed_retry`): concordo — facilita dashboard e decisão de reativação.
- ✅ `usage_logs` particionada por mês: DBA acerta em cheio. Mesmo com 30 usuários, chat com streaming gera muitas linhas. Particionamento + `DROP` para retention é o caminho.
- ✅ FK lógica (não física) em `usage_logs` particionada: concordo, com checagem aplicacional no boot (job que valida órfãos periodicamente).

**Auto-crítica**: reconheço que propus `tenant_id` no Turno 1 sem detalhar `DEFAULT 1`. DBA foi mais preciso. Alinho ao nome do DBA (`organization_id`) por consistência.

---

## 7. Frontend server-rendered (vs Frontend Architect) — facilita meu trabalho

Frontend rejeitou SPA e propôs Beego templates + HTMX + Alpine.js + Chart.js. **Como backend architect, isto é benção**:

**Por que facilita meu trabalho**:
1. **Sem API CRUD REST completa** para admin: telas admin consomem HTML sobre o wire via HTMX. Reduz drasticamente a superfície de API para desenhar/versionar/documentar.
2. **Sem JSON schema contract** para admin (exceto agregações de dashboard).
3. **Sessão server-side** (não JWT para UI): alinha com Security D5. Revogação trivial. Cookie httpOnly + CSRF — modelo clássico, testado.
4. **Separação clara de auth**: API do proxy usa Bearer `sk-...`; UI admin usa sessão. Não há conflito.

**Endpoints de agregação de dashboard — proposta concreta**:

Frontend pediu `GET /api/v1/web/dashboard/usage?from=&to=&granularity=&metric=&group_by=`. **Concordo com o shape**, com refinamentos:

- Path prefix `/api/v1/web/` (não `/v1/`, que é reservado ao proxy OpenAI-compat).
- Auth: **cookie de sessão** (não Bearer), via middleware Beego separado do middleware do proxy.
- Implementação: query direta em `usage_rollups_hourly` / `usage_rollups_daily` (tabelas que DBA desenhou). Para janelas curtas (< 24h), cair em `usage_logs` particionada com `date_trunc()` + `GROUP BY`.
- Granularidade adaptativa: `hour` se janela ≤ 7d, `day` se maior.
- Cache de 60s no handler (em memória) para queries de dashboard — evita re-agregar a cada clique.
- Limite de pontos: ≤ 365 (frontend concorda).

**Implementação backend**:
- `internal/web/controllers/dashboard.go` que valida sessão + role, lê parâmetros, chama `usage_repo.GetAggregated(...)`, serializa JSON.
- Endpoints adicionais (além do principal):
  - `GET /api/v1/web/dashboard/kpis?from=&to=` → contadores agregados (total reqs, tokens, taxa erro, chaves ativas/desabilitadas).
  - `GET /api/v1/web/admin/keys/{id}/status-history` → histórico de estados da chave (para a tela de gestão de chaves).
  - `POST /api/v1/web/admin/keys/{id}/force-enable` e `/force-disable` → admin override (requisito do briefing item 8).

**Embutir HTMX/Alpine/Chart.js como vendor-commit** (sem Node toolchain): concordo com Frontend. Mantém build único (`go build` com `//go:embed`) e deploy K8s simples (1 container, 1 binário).

---

## 8. Tensões menores resolvidas

| Ponto | Tensão | Resolução |
|---|---|---|
| Afinidade: persistir hash em DB ou só em memória? | Security recomenda só em memória (LGPD); DBA menciona `cached_tokens` em `usage_logs` só mede | **Só em memória com TTL curto (30 min)**. `cached_tokens` em logs apenas mede o benefício, não armazena fingerprint de conteúdo. Alinho com Security. |
| Sessions store: PostgreSQL ou Redis? | Security sugere PostgreSQL no MVP; DevOps sugere Redis em prod | **PostgreSQL no MVP** (tabela `sessions`). Redis quando multi-réplica chegar. |
| `audit_log` particionada? | DBA: não no MVP; Security: append-only com grants | **Não particionada no MVP**. Append-only via grants PostgreSQL (`GRANT INSERT` sem UPDATE/DELETE). Hash chaining opcional. |
| Logging structured: `slog` ou zerolog/zap? | Software Architect: `slog`; DevOps: `slog` ou zerolog/zap | **`slog`** (stdlib Go 1.21+). Zero dep adicional, suficiente para este volume. |
| `pgx` vs `database/sql` | DevOps: `pgxpool`; DBA: `database/sql` com SetMax* | **`pgx` v5 + `pgxpool`** (mais performático, tipos nativos PG). |

---

## 9. Tabela consolidada — posições revisadas

| Ponto | Posição Turno 1 | Posição Turno 2 (revisada) |
|---|---|---|
| Beego no hot path | Isolar | **Isolar** (concordo ADR-007; severidade do "bufferiza" um pouco exagerada) |
| Store abstraction | Interfaces sim, Redis não | **Interfaces sim, Redis não no MVP, deploy single-réplica explícito** (reconheço força do argumento DevOps sobre rate limit × N) |
| Graceful shutdown | ROADMAP | **Puxar para MVP** (concordo DevOps) |
| Token caching | DB hit | **DB hit, sem cache positivo** (confirmo Security D4) |
| Token hashing | Genérico | **HMAC-SHA256 com pepper** (Security) sobre SHA-256 puro (DBA) |
| Migration tool | golang-migrate | **golang-migrate + pgx v5 + pgxpool** (alinhado DBA/DevOps) |
| ORM/DB layer | pgx | **pgx direto, sqlc MVP+1, sem ORM** |
| `organization_id` | `tenant_id` nullable | **`organization_id NOT NULL DEFAULT 1`** (alinho DBA) |
| Schema granularidade | MVP enxuto | **~10 entidades + extensibilidade**, com `teams` opcionalmente adiado |
| `usage_logs` particionada | Não mencionei | **Concordo com DBA** (particionamento mensal) |
| Frontend | "TBD" | **Server-rendered HTMX** (concordo Frontend, facilita contrato de API) |
| Afinidade store | Em memória com TTL | **Só em memória com TTL 30min** (alinho Security sobre LGPD) |

---

## 10. Perguntas remanescentes para o consenso

1. **Para DevOps**: aceita `replicas: 1` em prod no MVP com SLA best-effort, ou insiste em Redis já no MVP para destravar HA real? Este é meu ponto de maior incerteza.
2. **Para Software Architect**: sobre pipeline de políticas (ADR-002) vs meu Selector+Chain separados — você enxerga o `FailoverPolicy` como uma policy dentro do pipeline (executada a cada tentativa, stateless) ou como orquestrador externo que invoca o pipeline N vezes (stateful entre tentativas)? Para mim, a segunda é mais correta porque failover mantém estado (chaves já tentadas, contagem). Sua posição?
3. **Para DBA**: confirma `pgx` v5 direto + `golang-migrate`, rejeitando GORM/sqlc no MVP? E sobre `teams`/`team_members` vazias no MVP — você manteria por conta do "quase grátis" ou aceitaria adiar?
4. **Para Security**: confirma envelope encryption (DEK por chave + KEK externo via ESO) como decisão final, com `encryption_key_id` para rotação lazy? Você vê risco em ter a KEK em Sealed Secrets (mais simples) vs External Secrets Operator (mais robusto) no MVP?

---

## 11. Auto-verificação do delta

1. **Mudei de posição em algo?** Sim, em 3 pontos: (a) store abstraction agora inclui interfaces explícitas mesmo sem Redis, (b) graceful shutdown saiu do ROADMAP para o MVP, (c) `organization_id NOT NULL DEFAULT 1` ao invés de nullable. As mudanças são bem fundamentadas pelos pares.
2. **Defendi posições que valia defender?** Sim: DB hit por request (vs cache positivo), HMAC com pepper (vs SHA-256 puro), `pgx` direto (vs sqlc/ORM), interfaces sem Redis (vs Redis no MVP).
3. **Identifiquei onde posso estar errado?** O ponto de maior incerteza é single-réplica em prod. Se DevOps for convincente, reviso para Redis no MVP.
4. **Há consenso emergente?** Sim, em vários pontos: hexagonal architecture, golang-migrate, pgx, server-rendered HTMX, organization_id desde o MVP, envelope encryption, graceful shutdown no MVP, audit_log append-only. As divergências que restam são menores.
