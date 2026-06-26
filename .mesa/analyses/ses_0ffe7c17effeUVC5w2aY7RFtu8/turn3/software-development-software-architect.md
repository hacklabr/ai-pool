# Posição Final de Consenso — Software Architect (Turno 3, posição 1/5)

> Articulo minha recomendação final sobre a única tensão material aberta (réplicas em prod) e **selo** os consensos já consolidados. Como primeiro a falar, estabeleço o chão técnico para que os pares possam concordar, refinar ou objetar.

---

## 1. Decisão central: Réplicas em produção — **RECOMENDO OPÇÃO A (1 réplica, sem Redis) para o MVP**

### 1.1 Por que A e não B

A bifurcação é real e DevOps a diagnosticou corretamente: **rate limit in-memory com N réplicas = limite × N**, o que é uma falha silenciosa de governança — inaceitável num produto cujo propósito é exatamente governança. DevOps concluiu daí que "2 réplicas + Redis" é obrigatório. **Eu rejeito a premissa intermediária** — não a conclusão de DevOps, mas a premissa de que prod *deve* ter 2 réplicas para esta escala.

**Raciocínio em três eixos:**

**(a) Deploy path é coberto mesmo com 1 réplica.** Com graceful shutdown agora no MVP (decidido pelo humano) + `strategy: RollingUpdate` com `maxSurge: 1` + `maxUnavailable: 0`, o K8s sobe o novo pod **antes** de derrubar o velho. O novo só recebe tráfego quando `readinessProbe` passa; o velho drena conexões em flight antes do SIGKILL. **Zero downtime planejado em deploy.** Isto neutraliza o argumento mais forte a favor de 2 réplicas.

**(b) Falha não-planejada tem custo aceitável.** A única vantagem residual de 2 réplicas é sobreviver a crash/OMM/node-failure sem a janela de restart do K8s (segundos a ~1 min). Para **30 usuários internos sem SLA contratual**, esta janela é aceitável. Não é um produto crítico de negócio 24/7; é uma ferramenta interna de governança de chaves. A troca "downtime de segundos em falha rara" ↔ "complexidade permanente de Redis + bugs de estado distribuído" é **desfavorável ao Redis** nesta escala.

**(c) Single-replica elimina uma classe inteira de bugs.** Com 1 pod:
- Rate limit conta corretamente (não ×N).
- Afinidade de cache de contexto funciona (não fragmenta por pod).
- Invalidez de cache de token ao desabilitar usuário é local e instantânea.
- Circuit breaker / cooldown de chave é coerente.
- LRU é global, não parcial.

Cada um destes é uma fonte potencial de bug sutil em multi-réplica que **não existe** em single-replica. Para um MVP, **menos modos de falha > HA cosmética**.

### 1.2 Implicações assumidas (transparência)

| Implicação de escolher A | Aceitabilidade |
|---|---|
| Downtime de segundos-minutos em crash do pod (até K8s reiniciar) | ✅ Aceitável — uso interno, sem SLA |
| Sem PDB com `minAvailable: 1` (não faz sentido com 1 réplica) | ✅ Aceitável |
| HPA fica desativado no MVP | ✅ Aceitável — 30 usuários |
| Ingress sticky sessions desnecessárias | ✅ Simplifica |
| Estado em memória perdido em restart (afinidade, LRU, rate counters) | ✅ Aceitável — LRU recria com tráfego; afinidade é otimização; rate limit reseta (burst permitido pós-restart, tolerável) |

### 1.3 Gatilho documentado para migrar A → B

Migrar para ≥2 réplicas + Redis **quando qualquer** destas condições:
1. **SLA contratual / HA hard requirement** aparece (uso além do time interno).
2. **RPS agregado sustentado > ~50/s** (carga real, não pico).
3. **Feature de quotas distribuídas** entra no roadmap ativo (exige contador cross-pod confiável).
4. **Multi-tenancy real** (múltiplas organizações externas) é especcada.

Na migração, `redisStore` (já esboçada pela interface) é implementada, `go-redis` é adicionado como dep, Helm chart sobe para `replicas: 2+`, PDB habilitado, NetworkPolicy egress para Redis adicionada. **O `Store` interface torna isto uma adição, não uma refatoração do hot path.**

### 1.4 Voto final

**Opção A: 1 réplica em prod para o MVP, sem Redis, com `Store` interfaces implementadas apenas em `memoryStore`.** Recomendo ao Manager adotar esta posição. Se o time de produto declarar HA como requisito (eixo b acima), reverto para B sem objeção — mas a decisão deve ser **explícita e consciente do trade-off**, não um default automático.

---

## 2. Consensos que SELLO (fechados, sem reabrir)

Estes pontos emergiram com convergência de ≥4 especialistas e **nenhuma objeção material**. Considero-os **decididos** e peço ao Manager que os registre como tal no spec:

### 2.1 Arquitetura e código
- **Hexagonal / Ports & Adapters** com `/internal/domain` puro (sem Beego, sem DB), adaptadores em `/internal/persistence`, `/internal/proxy`, etc. ✅
- **`pgx` direto (ou `pgxpool`)** — **NÃO** Beego ORM, **NÃO** GORM. Type-safety via `sqlc` como opção. ✅
- **Beego Controller no hot path com bypass de `ServeJSON`** + escrita direta em `ResponseWriter`/`Flusher`. Config de servidor streaming-aware (`WriteTimeout=0`, `ReadHeaderTimeout` curto). ✅ (concedi ao Backend)
- **Routing Engine = pipeline composável de políticas** (`HealthFilter` → `AffinityPolicy` → `LRUPolicy` → `FailoverPolicy`; futuras: `QuotaPolicy`, `SchedulePolicy`, `CapabilityPolicy`). ✅
- **ErrorParser = registry por provider** com `DefaultErrorParser` fallback (regex de data ISO/relativa + `Retry-After`). ✅
- **`Store` abstrações no MVP** (`LRUStore`, `AffinityStore`, `RateLimiter`, `KeyStateCache`, `TokenCache`) — interfaces definidas, **apenas `memoryStore` implementada**. ✅
- **Server-rendered HTMX + Alpine + Chart.js** para UI — **NÃO SPA**. Assets em `embed.FS`. ✅ (concordo com Frontend)
- **Imagem base `distroless/base-debian12:nonroot`** — **NÃO** `static` (CA certs para HTTPS de saída). Frontend deve alinhar. ✅ (DevOps correto)

### 2.2 Dados
- **`organization_id BIGINT NOT NULL DEFAULT 1`** em todas as tabelas multitenant + índices composite começando por `organization_id`. Seed de 1 org "default". ✅ (Security: alinhe de nullable para NOT NULL — é superior)
- **`usage_logs` particionada mensalmente desde dia 1** via pg_partman/CronJob, com pre-criação de partições (2 meses à frente) como **invariante operacional com alerta Prometheus**. Retention via `DROP TABLE` da partição (90 dias default). ✅ (concedi a DBA)
- **`golang-migrate`** como ferramenta de migrations; via Helm pre-upgrade Hook Job (não ORM auto-migrate, não manual). ✅
- **Envelope encryption AES-256-GCM** para `provider_keys` (DEK por chave + KEK externo via ESO/Sealed Secrets). `encryption_key_id` para rotação. ✅
- **Tokens `sk-...` com `key_id` público + HMAC-SHA256(pepper)** do secret; lookup O(1) por `key_id`. ✅
- **`model_aliases`, `model_pricing`, `cost_usd`, `cached_tokens`, `context_window`** — criar no MVP (tocam roteamento/hot-path ou são colunas únicas). ✅
- **DEFERIR** `teams`, `team_members`, `quotas`, `time_window_limits` (standalone vazias) para quando a feature for especcada. ✅ (peço ao DBA aceitar — schema morto diverge do código)

### 2.3 Segurança e identidade
- **Sessão server-side** (cookie `httpOnly; Secure; SameSite=Strict`) para UI admin — **NÃO JWT**. Store: tabela em PG (MVP) → Redis (escala). ✅
- **`sk-...` só autentica API do proxy** (`/v1/*`); UI admin usa sessão própria. Separação absoluta. ✅
- **`argon2id`** para senhas (memory=64MB, iterations=3, parallelism=2). ✅
- **Cache de token positivo 15s + purge explícito** ao desabilitar usuário/revogar token. (Security: peço que aceite este trade-off vs DB-hit por request — RPS agentic inviabiliza DB-hit puro; purge dá revogação near-instant na prática.) ✅
- **Allowlist positiva de headers repassados** + SSRF guard (bloquear RFC1918, loopback, link-local, metadata IP). ✅
- **`SanitizingLogger`** central; **NUNCA logar** chaves, Authorization, nem conteúdo de mensagens (default). ✅
- **`audit_events` append-only** (INSERT-only grants) + hash chaining opcional. ✅

### 2.4 Infra e operação
- **Graceful shutdown no MVP** (Fase 3): SIGTERM → parar Accept → `srv.Shutdown(ctx 30s)` → esperar streams em flight. ✅ (decidido pelo humano)
- **Docker multi-stage** (builder `golang:1.22-alpine` → runtime `distroless/base-debian12:nonroot`), BuildKit cache, `.dockerignore`. ✅
- **Helm chart** com Deployment, Probes (startup/liveness/readiness), Service, Ingress (nginx com `proxy-buffering: off` — **mandatório para SSE**), ConfigMap, ExternalSecret, NetworkPolicy (DenyAll default + egress explícito DB/DNS/provedores). ✅
- **Managed DB em prod** (CloudSQL/RDS/Azure) com backup + PITR; CNPG se on-prem. ✅
- **CI/CD** GitHub Actions: lint → test → security-scan (gosec/govulncheck/trivy) → build → scan-image → deploy-staging → manual-prod. OIDC federation. ✅
- **OpenTelemetry instrumentation no MVP** (collector depois); **Prometheus instrumentation** no MVP (`/metrics` desativado default, liga em 1 commit). ✅
- **JSON structured logging** (`log/slog`), stdout/stderr. ✅
- **`pgxpool`** com limites calibrados; PgBouncer só se réplicas crescerem. ✅

### 2.5 Produto/compatibilidade
- **Contrato de compatibilidade OpenAI como entregável obrigatório do spec** — quais endpoints (`/v1/chat/completions`, `/v1/models`), quais campos passados ipsis litteris, quais features best-effort (`stream_options.include_usage`), shape de erro devolvido ao cliente. ✅ (lacuna coletiva; reforço)
- **Failover só pré-primeiro-byte** para streaming — erro pós-stream é terminal ao cliente. Documentar como limitação aceita. ✅
- **Afinidade de cache de contexto**: `X-Conversation-ID` header é mecanismo **primário e documentado como contrato**; hash de prefixo é **fallback puro** com observabilidade de hit-rate. Semântica "último writer vence" no store (idempotente por conversa). Best-effort: se chave sticky em cooldown, cai para LRU. ✅

---

## 3. Pontos onde ainda sustento divergência (peço voto do Manager)

1. **Réplicas = 1 no MVP** (vs DevOps: 2 + Redis). **Voto: A.** Justificado em §1.
2. **Cache de token 15s + purge** (vs Security: DB-hit por request). **Voto: cache.** Justificado em T2 §3.2.
3. **Deferir tabelas standalone vazias** (vs DBA: criar agora). **Voto: deferir.** Justificado em T2 §2.5b.
4. **Graceful shutdown na Fase 3** (já decidido pelo humano — confirmo alinhamento).

## 4. Convite aos pares

Peço aos próximos 4 especialistas que se posicionem **especificamente** sobre:
- **Réplicas A vs B** (voto direto, com objeção se houver).
- **Cache de token** (Security especialmente).
- **Tabelas standalone vazias** (DBA especialmente).
- **Confirmação dos selos** em §2 (objeções pontuais, não reabertura geral).

Se houver objeção material a qualquer selo em §2, abro discussão. Caso contrário, considero o consenso técnico **formado** e pronto para especificação.

---

## 5. Síntese executiva para o Manager

A arquitetura **convergiu**. As 5 análises + 2 deltas formam um design coeso. As únicas decisões que precisam de arbitragem são as 3 de §3 — e em todas elas minha recomendação pende para **simplicidade no MVP** (1 réplica, cache leve, deferir schema morto), com **gatilhos documentados para evoluir**. O custo de começar simples é baixo porque as abstrações (`Store` interface, `organization_id`, particionamento, routing pipeline) já pavimentam a evolução sem rewrite. Recomendo ao Manager **aceitar os selos de §2 como decididos** e **submeter as 3 votações de §3 ao time**, adotando minha recomendação como default na ausência de objeção técnica fundamentada.