# Delta DevOps (Turn 2) — Alinhamento com os Pares

Li integralmente as 5 análises dos pares. Há forte convergência em quase tudo; ajusto posições onde o consenso é claro e mantenho divergências honestas onde o trade-off é genuíno.

---

## 1. Statefulness / multi-réplica — ACEITO o consenso

**Convergência**: Software Architect (ADR-006, §4.5, §7.3), Backend Architect (§4.5, §13), DBA (§2.8, §12.5) e Security (§4.4) convergem para **MVP = 1 réplica em produção**. Estado em memória (LRU, affinity, rate limit, token cache) é aceitável neste contexto.

**Retiro minha posição mais agressiva do Turn 1 (§9.3 — "Redis no chart para multi-réplica em prod")**: para 30 usuários internos, a complexidade operacional de Redis não se paga. A interface abstrata é o ponto ótimo — barata de escrever agora, cirúrgica de trocar depois.

**O que mantenho como INEGOCIÁVEL**: a **abstração de store via interfaces** deve entrar no MVP. Concordo integralmente com Backend Architect §13 e Software Architect ADR-006: `LRUStore`, `AffinityStore`, `RateLimiter`, `TokenValidator` como interfaces; `MemoryXxx` como implementação concreta no MVP; `RedisXxx` no ROADMAP. Isto **não** é YAGNI — é o custo simbólico de evitar rewrite do hot path quando o ROADMAP chegar.

**YAGNI check honesto**: a interface abstrata custa ~1-2 dias de trabalho (definir interfaces + 2 implementações memory). O Redis implementado custaria ~1 semana + 1 nova dependência operacional. **A relação custo/benefício favorece claramente só a interface no MVP**. Alinho com Backend/Software.

**Gatilho explícito para documentar no ROADMAP (quando mudar de ideia)**:
- > 1 réplica por HA (não por carga — para 30 usuários carga nunca justifica).
- Quotas cross-pod distribuídas (ROADMAP de quotas).
- Invalidação imediata cross-pod de tokens revogados (Security §4.4 levanta isso; no MVP single-pod é trivial via `tokenCache.PurgeUser`).

---

## 2. Graceful shutdown no MVP — consenso total, reforço o impacto

Software Architect (§4.4, §10) e Backend Architect (§13) recomendam puxar do ROADMAP. **Concordo integralmente.**

**Impacto concreto que justifica a prioridade**:
- Com **1 réplica + strategy: Recreate** (sem graceful shutdown), cada deploy = **downtime explícito** de ~10-30s (tempo de shutdown do pod velho + startup do novo + migrations).
- Com **RollingUpdate maxSurge: 1, maxUnavailable: 0** (sem graceful shutdown), 2 pods coexistem por alguns segundos; quando o velho recebe SIGTERM, qualquer stream SSE em curso é **abortado abruptamente** — cliente vê a conexão cair no meio da resposta do LLM. Para um produto cuja UX principal é chat com streaming, isso é **visível e irritante**.
- Beego mantém um `app.Server` (`net/http.Server`); implementar é direto: capturar `SIGTERM`, `srv.Shutdown(ctx)` com timeout 30s, aguardar `sync.WaitGroup` de streams em curso terminarem (ou cancelá-los após o timeout).

**Esforço estimado**: 0.5-1 dia de trabalho (signal handler + WaitGroup + teste com `curl -N` durante deploy). Vale MUITO a pena.

**Respondendo Backend Architect §14.1 (sobre `terminationGracePeriodSeconds`)**:
- Com graceful shutdown implementado: `terminationGracePeriodSeconds: 45` (margem de 15s acima do `srv.Shutdown` interno de 30s). K8s envia SIGTERM → app fecha listener (não aceita novas) → drena em curso → exit clean em <30s. K8s raramente precisa mandar SIGKILL.
- Sem graceful shutdown: 30s default do K8s, mas streams são cortados — cenário a evitar.

---

## 3. Helm chart single-replica — REVERTO minha recomendação

Com o consenso de **1 réplica no MVP**, meu Turn 1 §3.2 (`replicas: 2`) está incorreto. Reviso:

**Removo do chart MVP**:
- **HPA** — não faz sentido (min replicas = 1, não há nada para escalar; escala vertical resolve se CPU sobrar).
- **PDB** — com 1 réplica é inútil: `minAvailable: 1` bloqueia `kubectl drain` (você nunca consegue fazer manutenção); `maxUnavailable: 1` permite downtime total (sem efeito protetor). Nenhum dos dois ajuda.
- **`topologySpreadConstraints` / `podAntiAffinity`** — sem efeito com 1 réplica.

**Mantenho (essenciais mesmo com 1 réplica)**:
- **Probes**: startup (cold start com migrations via init-container pode demorar), liveness (auto-restart em deadlock de goroutines de streaming — risco real, Backend Architect §12.1), readiness (não recebe tráfego enquanto DB indisponível). Sempre.
- `revisionHistoryLimit: 10` para rollback.
- `securityContext` completo (runAsNonRoot, readOnlyRootFS, drop ALL caps, seccomp RuntimeDefault) —constraint do role.
- `resources.requests/limits` — constraint do role.
- NetworkPolicy DenyAll + liberações explícitas.
- `strategy: Recreate` OU `RollingUpdate maxSurge: 1, maxUnavailable: 0` (com graceful shutdown, RollingUpdate é seguro; sem ele, Recreate é mais honesto sobre o downtime).

**Comentário no `values.yaml`**: HPA/PDB/topologySpread devem ser habilitados **junto com** Redis quando escalar para multi-réplica. Chart GitOps-ready.

---

## 4. Secret store — ALINHO com Security Architect

Security Architect §9.1 recomenda External Secrets Operator (gold standard), Sealed Secrets (OK) ou Cloud KMS + secret mount como arquivo (não env var). **Concordo integralmente.**

**Adoção crucial da recomendação do Security**: KEK (master key de criptografia envelope) montada como **arquivo em volume readOnly** (`mountPath: /etc/aipp-secrets`, `readOnly: true`, `defaultMode: 0400`), **não como env var**. Justificativa técnica forte: env vars aparecem em `/proc/<pid>/environ` (acessível a quem tem acesso ao node/pod, mesmo sem escalonamento de privilégios), enquanto secret volumes montados como arquivo tmpfs têm exposição menor. É defesa em profundidade que custa zero.

**Decisão final consolidada (alinhada DevOps + Security + DBA)**:

| Tipo de secret | Estratégia MVP |
|----------------|----------------|
| DB password (`DATABASE_URL`/`DB_PASSWORD`) | ExternalSecret → materializado como Secret K8s → montado como env var OU arquivo (preferir arquivo) |
| Session secret (cookie web admin) | ExternalSecret → env var |
| **KEK (master key, criptografia envelope das chaves de provedores)** | ExternalSecret → Secret K8s → **volume readOnly, `defaultMode: 0400`**, lido do arquivo `/etc/aipp-secrets/kek` |
| Pepper HMAC (tokens sk-...) | ExternalSecret → env var |
| **Chaves de provedores (Gemini, Mistral, etc.)** | **CIFRADAS no PostgreSQL** (AES-256-GCM envelope com DEK/KEK). NÃO são secrets de infra. Gerenciadas via CRUD admin. |

**Fallback on-prem** (briefing não determina cloud): Sealed Secrets (Bitnami) ou HashiCorp Vault + ESO.

**Respondendo Security §13.4** (pergunta sobre cloud específico): briefing não determina. Vou propor no spec: **default = cloud-managed** (assume AWS ou GCP — ESO + Secrets Manager/KMS nativo), com instrução de adaptação para on-prem (Sealed Secrets + CNPG) documentada no README do chart.

---

## 5. Migrations no deploy — CONFIRMO e detalho

**Consenso total** em **golang-migrate** (Backend Architect §10, Software Architect §4.3, DBA §8.1, meu Turn 1 §5.3). Nenhuma divergência.

**Integração no deploy — detalhamento concreto**:

- **Produção**: Helm `pre-install, pre-upgrade` Hook rodando um **Job** que executa:
  ```bash
  /app migrate -path file:///migrations -database "$DATABASE_URL" up
  ```
  - `restartPolicy: OnFailure`.
  - `activeDeadlineSeconds: 300s` (5 min — migrations lentas alertam).
  - Vantagem: roda **UMA VEZ** antes do Deployment. Mesmo com 1 réplica no MVP, isso prepara para o ROADMAP multi-réplica (sem race).
- **Dev (docker-compose)**: init-container no serviço app, executando migrations antes de subir o servidor. Mais simples para iterar localmente.
- **Migrations embedded no binário** via `embed.FS` (Backend Architect §10) — evita dependência de arquivos externos no container, mantém imagem distroless limpa.

**Reflexo da disciplina do DBA §8.2**: o Helm Job NÃO roda `down` automaticamente (rollback de migration é manual, documentado, planejado). Política de PR obrigatório para toda migration; revisão focada em `CREATE INDEX CONCURRENTLY`, `ADD COLUMN ... NULL` antes de `SET NOT NULL VALIDATE`, etc.

---

## 6. Ingress + SSE — CONFIRMO as annotations, sem conflito com Frontend

Backend Architect §3.4 confirma a mecânica de streaming SSE (handler HTTP puro fora do Controller Beego, `http.Flusher`, headers `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`).

Frontend Architect recomenda server-rendered Beego com assets `embed.FS` — **NÃO há conflito**, porque as duas superfícies são distintas:
- **Proxy path** `/v1/*` (chat/completions) → streaming SSE para clientes LLM (SDK OpenAI, curl -N, apps).
- **Admin/dashboard** `/dashboard`, `/api/v1/web/*` → server-rendered HTML + Chart.js consumindo JSON agregado. Não é streaming.

**Annotations de Ingress (nginx) — confirmadas, justificadas**:
```yaml
nginx.ingress.kubernetes.io/proxy-buffering: "off"        # ESSENCIAL p/ SSE; sem isso nginx bufferiza e quebra streaming
nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"    # 1h p/ respostas longas (chat longo)
nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
nginx.ingress.kubernetes.io/configuration-snippet: |
  proxy_http_version 1.1;                                  # SSE exige HTTP/1.1
  proxy_set_header Connection "";                          # desabilita keepalive upstream pooling quebrado
```

**Belt-and-suspenders (importante)**: o Backend Architect recomenda setar `X-Accel-Buffering: no` no header de resposta da app (instrui o nginx a não bufferiar **essa resposta específica**). A annotation `proxy-buffering: off` no Ingress desabilita buffering **globalmente** para o Ingress. Manter **ambas** — algumas versões/configurações do nginx respeitam uma ou outra, e o custo é zero. Alinhei com Backend Architect.

---

## 7. Divergências honestas e correções a registrar

### 7.1. PostgreSQL em produção: Managed DB (meu Turn 1) vs CNPG (DBA) — divergência genuína

- Eu (Turn 1 §5.1): **Managed DB** (CloudSQL/RDS/Azure DB).
- DBA §9.2: **CloudNativePG (CNPG)** — operator Postgres nativo K8s, com WAL archiving, barman, streaming replication, PITR.

**Análise do trade-off (honesta)**:

| Critério | Managed DB | CNPG (self-hosted) |
|----------|-----------|---------------------|
| Custo operacional | **Baixo** (cloud gerencia backups, failover, patches) | Médio (operator + você monitora PVCs, WAL archive, etc.) |
| Backups/PITR | Incluso nativamente | Configurável (barman → S3/MinIO; funciona bem) |
| HA/Failover | Incluso (multi-AZ) | Streaming replication + operator failover (bom, mas você debuga) |
| Custo financeiro | Pago por hora (geralmente > self-host para workloads pequenos) | PVCs + CPU/RAM do cluster (geralmente **mais barato** para workloads pequenos) |
| Lock-in cloud | Alto | Baixo (CNPG roda em qualquer K8s) |
| Latência app↔DB | Rede cloud (pode ter 1-3ms RTT) | Intra-cluster (melhor, <1ms) |
| Skills necessárias | DBA básico | SRE + DBA (operator expertise) |

**Decisão proposta para o spec — deixar como variável dependente do ambiente, não imponho minha visão**:
- **Default (cloud-first)**: Managed DB — menor custo operacional, equipe foca no produto.
- **Alternativa (self-hosted/cloud-agnostic/on-prem)**: CNPG — se houver expertise K8s/SRE no time, requisito de portabilidade entre clouds, ou workload já consolidado em K8s.

**Respondendo DBA §13** ("validar com DevOps: adoção de CNPG"): ambos aceitáveis. Peço que o spec **explicitamente declare o ambiente alvo** (cloud específico vs on-prem vs portátil) para fixar a escolha. Caso não haja decisão, proponho **default managed** para reduzir risco operacional em MVP.

### 7.2. CORREÇÃO técnica ao Frontend Architect: distroless `static` vs `base`

Frontend Architect §7 recomenda `FROM gcr.io/distroless/static`.

**Correção importante**: `distroless/static` **NÃO tem CA certificates** (`/etc/ssl/certs/`). O proxy precisa falar HTTPS com provedores externos (Gemini, Mistral, DeepSeek, Z.ai, Kimi) — sem CA roots, TLS verification falha (`x509: certificate signed by unknown authority`).

**Recomendação correta**: `gcr.io/distroless/base-debian12:nonroot` (tem `/etc/ssl/certs/ca-certificates.crt` + UID 65532 já configurado). Já estava no meu Turn 1 §2.1. **Solicito ao Frontend alinhar com esta escolha**, pois afeta o Dockerfile que ambos referenciamos.

Caso se queira otimizar tamanho com `distroless/static`, seria necessário `COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/` no Dockerfile — manutenção extra, geralmente não vale.

### 7.3. Respondendo Backend Architect §14.1 — stack de logs

Backend pergunta: "Logs: JSON estruturado para qual stack? (Loki/ELK/CloudWatch)".

**Resposta DevOps definitiva**:
- **MVP recomendado**: `stdout/stderr` (12-factor) → **Fluent-bit** (DaemonSet) → **Loki** → **Grafana**. Stack leve, barata, K8s-nativa, já alinha com o stack Prometheus/Grafana quando o `/metrics` chegar (ROADMAP). Single pane of glass.
- **Alternativa cloud (se já houver)**: Cloud Logging (GCP) / CloudWatch Logs (AWS) — zero ops, mas vendor lock-in e custo pode crescer.
- **Evitar**: ELK clássico (Elasticsearch consome muitos recursos) para 30 usuários é overkill. Loki é propositalmente mais leve (não indexa conteúdo, só labels).

### 7.4. Confirmação ao DBA sobre scheduler de reativação

DBA §5.2 propõe `SELECT ... FOR UPDATE SKIP LOCKED` para o scheduler de reativação.

**Confirma DevOps**: design correto. Com **1 réplica no MVP** não há concorrência real entre workers, mas o `SKIP LOCKED` já previne race quando o ROADMAP trouxer multi-réplica — **não precisa mudar a query depois**. Forward-compatible.

### 7.5. Confirmação ao Backend Architect sobre `http.Client` tuning

Backend Architect §3.3 descreve o tuning detalhado do `http.Client` (sem `Client.Timeout` global, control via `context`, `ResponseHeaderTimeout: 30s`).

**Confirma DevOps**: alinha perfeitamente com minha recomendação de timeouts (Turn 1 §8.2). Para o chart, isso se traduz em: nenhum timeout no nível do Ingress que conflite com esses valores (por isso `proxy-read-timeout: 3600` alto no Ingress — não queremos que o nginx corte streams longos que a app está gerenciando com idle timeout de 60s entre chunks).

---

## 8. O que NÃO muda do meu Turn 1 (mantenho integralmente)

Após revisão pelos pares, **nada do meu Turn 1 precisa ser revertido** além dos pontos acima (1 réplica, sem HPA/PDB, sem Redis no MVP). Mantenho:

- ✅ Dockerfile multi-stage distroless (`base-debian12:nonroot`).
- ✅ `.dockerignore` + BuildKit cache mounts.
- ✅ docker-compose com PostgreSQL + volume + healthchecks + hot reload via `air`.
- ✅ CI/CD GitHub Actions com security gates (gosec/govulncheck/trivy) + OIDC + push-based.
- ✅ Tags imutáveis (SHA digest); proibido `latest`.
- ✅ Health endpoints `/healthz`, `/readyz`, `/version`.
- ✅ JSON structured logging desde o MVP (`log/slog`).
- ✅ NetworkPolicy DenyAll + liberações explícitas (egress para DB + provedores + DNS).
- ✅ Resource requests/limits + securityContext completo.
- ✅ Migrations via Helm Job hook com golang-migrate.
- ✅ Provider keys cifradas no DB (envelope encryption DEK/KEK).
- ✅ OpenTelemetry tracing instrumentation desde o MVP.
- ✅ Prometheus instrumentation desativada (env var `METRICS_ENABLED=false`) mas presente no código.
- ✅ Runbook documentado para cada alerta.
- ✅ `/metrics` no ROADMAP mas com `prometheus/client_golang` já no código.

---

## 9. Tabela de convergências e divergências

| Tópico | Turn 1 (meu) | Pares | Turn 2 (alinhado) |
|--------|--------------|-------|-------------------|
| Réplicas MVP | 2 + Redis-ready | 1 (consenso total) | **1 réplica, interfaces abstraídas, Redis no ROADMAP** |
| Graceful shutdown | MVP+1 | MVP (Software, Backend) | **MVP** (consenso, ~1 dia esforço) |
| HPA/PDB no chart MVP | Sim | n/d | **Não** (sem efeito com 1 réplica); comentado para ROADMAP |
| Secret store | ESO + env vars | ESO + arquivo volume (Security) | **ESO + KEK como arquivo readOnly** (alinho com Security) |
| Migrations tool | golang-migrate | golang-migrate (unânime) | **golang-migrate** (sem divergência) |
| Migrations deploy | Helm Job hook | DBA: Job OU CI; Backend/Software: n/d | **Helm pre-install/pre-upgrade Job hook** (produção), init-container (dev) |
| Ingress SSE | Annotations | Backend confirma mecânica | **Annotations confirmadas + `X-Accel-Buffering: no` belt-and-suspenders** |
| PostgreSQL prod | Managed DB | DBA: CNPG | **Variável por ambiente** (default managed; CNPG se on-prem/SRE) |
| Imagem runtime | distroless base-debian12 | Frontend: distroless static | **distroless base-debian12** (correção ao Frontend — CA roots) |
| Logs stack | Loki/Fluent-bit | Backend pergunta | **Loki + Fluent-bit + Grafana** (MVP) |
| Provider keys | Cifradas no DB | Consenso total | **Cifradas no DB (envelope)** |

---

## 10. Resumo do delta

1. **MVP = 1 réplica**: aceito o consenso. Retiro pressão por Redis. Mantenho interface abstraida de store como inegociável (Redis-ready, ~1-2 dias de trabalho).
2. **Graceful shutdown no MVP**: consenso total. Reforço impacto em streaming (deploy corta chat). Esforço ~1 dia. `terminationGracePeriodSeconds: 45`.
3. **Helm chart simplificado**: sem HPA/PDB/topologySpread (inúteis com 1 réplica). Probes mantidas. Comentado para habilitar junto com Redis no ROADMAP.
4. **Secrets**: External Secrets Operator + cloud secrets manager (default). KEK como arquivo readOnly volume (alinho com Security).
5. **Migrations**: golang-migrate via Helm Job hook (prod) + init-container (dev). Consenso unânime.
6. **Ingress SSE**: annotations confirmadas; belt-and-suspenders com `X-Accel-Buffering: no`.
7. **Divergência honesta**: PostgreSQL managed vs CNPG — variável por ambiente, default managed.
8. **Correção ao Frontend**: `distroless/base-debian12`, não `distroless/static` (CA roots para HTTPS).

Pronto para turno de consenso.