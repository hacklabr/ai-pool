# DECISIONS — Decisões de Execução

> Registro das decisões tomadas durante a implementação do MVP. Inclui decisões
> arquiteturais aprovadas pelo consenso dos especialistas (Turno 3) e decisões
> operacionais tomadas autonomamente pelo **Manager** durante a execução noturna
> (após autorização explícita do humano).

## Decisões de arquitetura (consenso dos especialistas — 6-0)

| # | Decisão | Justificativa |
|---|---------|---------------|
| A1 | **1 réplica em produção, sem Redis** (Opção A) | Elimina bugs de estado distribuído para 30 usuários sem SLA. Store abstrações (`LRUStore`, `AffinityStore`, `RateLimiter`, `TokenCache`) no MVP, só `memoryStore` implementada, **sem importar `go-redis`**. RollingUpdate `maxSurge:1`/`maxUnavailable:0` + graceful shutdown = zero-downtime planejado. |
| A2 | **Graceful shutdown no MVP** (puxado do ROADMAP) | ~50 linhas; sem ele, deploys cortam streams de chat em andamento. Recomendação unânime (6-0) aceita pelo humano. |
| A3 | **Arquitetura Hexagonal** (Ports & Adapters) | `domain/` puro, Beego como detalhe de transporte, stores abstraídos por interfaces. |
| A4 | **Proxy core em HTTP puro** (isolado do Beego) | Beego pode bufferizar e quebrar SSE. Proxy usa `net/http` + `http.Flusher`; Beego só para admin/UI. |
| A5 | **`pgx` v5 direto** (sem GORM/sqlc) | Controle fino, performance. |
| A6 | **`golang-migrate`** via Helm Job hook | Versionamento de schema. |
| A7 | **`organization_id NOT NULL DEFAULT 1`** em todas as tabelas | Pavimenta multi-tenancy sem migração cara (especialmente em `usage_logs` particionada). |
| A8 | **HMAC-SHA256 + pepper** para tokens (não SHA-256 puro, não Argon2) | Defesa em profundidade; lookup O(1) via `key_id`; alta entropia dispensa hash lento. |
| A9 | **DB hit por request** para validação de token (sem cache) | Revogação instantânea e determinística; latência sub-ms é ruído vs LLM call. |
| A10 | **Envelope encryption AES-256-GCM** com KEK externo via ESO | KEK como arquivo readOnly, nunca env var. |
| A11 | **Frontend server-rendered** (html/template + HTMX + ECharts), NÃO SPA | 30 usuários internos; binário único com embed.FS; menos superfície de ataque. |
| A12 | **distroless/base-debian12:nonroot** | CA roots p/ HTTPS com provedores (`static` não serve). |
| A13 | **External Secrets Operator (ESO)** para KEK | Defesa em profundidade. |
| A14 | **usage_logs particionada mensal** + rollups em tabelas com `cached_tokens` | Volume alto; dashboard em ms. |
| A15 | **Audit_log append-only** | Imutabilidade. |

## Decisões operacionais tomadas pelo Manager (autônomas)

> Estas decisões são **configuráveis/reversíveis**. O humano pode ajustá-las.

### D1 — Timeouts do proxy
- first-byte: **10s** (até primeiro chunk útil do stream)
- idle: **30s** (entre chunks)
- overall: **120s** (tempo total)
- Arquivo: `internal/config/config.go` + `proxy/stream.go`

### D2 — Parâmetros de failover
- backoff base: **200ms**, cap: **5s**, **full jitter** (`crypto/rand`)
- failover em: 401/403/429/5xx/erros de rede/timeout
- NÃO failover em: 400/404/413
- sem failover após primeiro chunk útil do stream
- Arquivo: `internal/routing/failover.go`

### D3 — Auto-disable
- threshold: **5 erros** em janela de **2 min** → `auto_disabled`
- backoff crescente (1min/5min/15min/1h/6h/24h) quando o parser não encontra data
- reativação: scheduler 30s (`FOR UPDATE SKIP LOCKED`) + lazy
- Arquivo: `internal/routing/auto_disable.go`

### D4 — Afinidade de cache de contexto
- TTL: **30 min** por usuário
- `X-Conversation-ID` (header) preferido; fallback hash SHA-256 do prefixo normalizado
- prefixo estrito (n > k)
- hash **jamais** logado/persistido
- Arquivo: `internal/adapters/store/affinity_memory.go`, `internal/routing/affinity_hash.go`

### D5 — Rate limiting
- proxy: **60 req/min por token** (configurável)
- login: **10 req/min por IP** (anti brute-force)
- Arquivo: `proxy/server.go`, `admin/login.go`

### D6 — Registro de uso
- batch insert: **100 eventos OU 1s**
- canal bufferizado cap 10000, `Record` não-bloqueante (descarta se cheio)
- dead-letter individual em falha de insert
- rollup job: **5 min**, agrega hora fechada, idempotente (`ON CONFLICT DO UPDATE`)
- Arquivo: `internal/usage/`

### D7 — Sessões admin
- session store em memória (map+RWMutex), TTL **12h**, lazy eviction + cleanup ativo 10min
- cookie `aipp_sess` HttpOnly+Secure(condicional dev)+SameSite=Lax
- CSRF double-submit: cookie `aipp_csrf` + header `X-CSRF-Token` + validação contra `sess.CSRFToken`
- Arquivo: `internal/adapters/store/session_memory.go`, `admin/middleware.go`

### D8 — KEK & criptografia
- KEK: **32 bytes** (AES-256) via `AIPP_KEK_PATH` (arquivo readOnly)
- rotação: **anual** sugerida (procedimento documentado em `internal/adapters/crypto/envelope.go`)
- múltiplos `encryption_key_id` suportados (rotação gradual)
- DEV_MODE sem KEK → `IdentityDecryptor` (plaintext) com WARN loud
- formato encrypted_value: `nonce(12) || ciphertext || tag(16)`

### D9 — Retenção de dados
- `usage_logs`: **DROP de partição aos 90 dias** (retenção sugerida; revisar política LGPD)
- `audit_log`: append-only (sem retenção automática definida)

### D10 — Particionamento automático
- **CronJob Kubernetes** (não `pg_partman`) para pré-criar partições mensais
- Justificativa: K8s-native, já estamos no cluster. (Ainda a implementar como job periódico.)

### D11 — Tenant default
- org `id=1` nome **"default"** (seeded na migration F2)
- Onboarding de usuários: via bootstrap admin CLI ou admin UI

### D12 — DB produção
- **Managed DB** (CloudSQL/RDS) como default, com backup + **PITR inegociável**
- **CNPG** (CloudNativePG) como exceção para on-prem
- Helm chart assume DB externo (não inclui StatefulSet postgres)

### D13 — Observabilidade
- logging: JSON estruturado sanitizado (`SanitizingLogger`)
- stack recomendada: **Loki + Fluent-bit + Grafana**
- Prometheus: instrumentation no código (6 métricas), endpoint `/metrics` **gated** (`ENABLE_METRICS`, default `false` no MVP)
- tracing: OpenTelemetry prepado (request_id/trace_id em logs)

### D14 — CI/CD
- **GitHub Actions**: go vet + test -race + gofmt + gosec + govulncheck + trivy (fail HIGH/CRITICAL) + helm lint/template
- release on tag `v*` → push `ghcr.io/rafael/ai-pool` via **OIDC** (sem PAT)
- deploy staging: template comentado (sem cluster real)
- Makefile target `ci-local`

## Decisões de design emergentes (especialistas durante implementação)

- **Timing equalization no login**: usuário inexistente compara senha contra `fakeHash` Argon2id (anti-enumeration).
- **Admin não pode desabilitar a si mesmo**: previne lockout.
- **Anti-IDOR em tokens**: valida ownership antes de revogar.
- **Token format**: `sk-aipp-<keyid(8)>-<secret>`, key_id visível, secret mascarado na UI.
- **CSS embed, JS via CDN com SRI**: CSS próprio self-contained; HTMX/Alpine/ECharts via CDN (pragmático para 30 usuários; fallback vendor-commit documentado).
- **Login multi-step**: Alpine `step` com slot OAuth reservado.
