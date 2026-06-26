# Analysis: DevOps Engineer — Turn 1

**Agent ID:** software-development-devops-engineer
**Timestamp:** 2026-06-25T18:48:57.860Z

# Análise de Infraestrutura — AI Provider Pool (DevOps Engineer)

## 1. Visão Geral

O briefing define um proxy/load-balancer de LLMs em **Go + Beego + PostgreSQL** com dois ambientes: Docker Compose (dev/teste local) e Kubernetes + Helm Chart (produção). A escala alvo é **pequena** (~30 usuários internos, ~5 provedores, 1-5 chaves por provedor) — isso muda o cálculo: o objetivo NÃO é hiper-escala, é **HA leve + governança + observabilidade básica + deploy reproduzível**.

Como DevOps Engineer, identifiquei **um risco arquitetural crítico que o briefing NÃO endereça explicitamente**: o proxy NÃO é puramente stateless no MVP (LRU das chaves, rate limit por usuário/token e afinidade de cache de contexto são estados em memória). Isso impacta diretamente a escalabilidade horizontal e precisa de decisão antes do deploy em produção.

---

## 2. Docker (desenvolvimento/teste local)

### 2.1 Dockerfile — multi-stage para Go

Recomendo **multi-stage build com 2-3 estágios**:

- **Builder**: `golang:1.22-alpine` (versão pinada conforme `go.mod`). Padrão de cache de layers: copiar primeiro `go.mod`/`go.sum` e rodar `go mod download`, depois copiar o restante do código. Comando de build:
  ```
  go build -ldflags="-s -w" -trimpath -o /out/app -buildvcs=false
  ```
  - `-s -w`: remove símbolos de debug (imagem menor).
  - `-trimpath`: remove paths absolutos (reprodutibilidade).
  - `CGO_ENABLED=0`: binário estático puro (essencial para distroless/scratch).

- **Test/Lint stage** (opcional no `Dockerfile`, obrigatório no CI): `go vet`, `go test -race ./...`, `golangci-lint`, `govulncheck`.

- **Runtime**: **`gcr.io/distroless/base-debian12:nonroot`** (não `static-debian12`).
  - Razão para `base` em vez de `static`: o proxy fala HTTPS com provedores externos → precisa de CA certificates (`/etc/ssl/certs`) que só a `base` traz.
  - `:nonroot` traz UID/GID `65532` pré-configurado → satisfaz `runAsNonRoot` sem configurar UID manualmente.
  - Sem shell, sem package manager → superfície de ataque mínima.

**Trade-offs descartados**:
- **scratch**: não tem CA roots → quebra HTTPS para provedores. Apenas viável se embutirmos CA bundle via `COPY` (extra, manutenção) — descartado para MVP.
- **alpine**: tem shell (bom para debug), mas musl libc pode gerar problemas com CGO/race detector; produção não precisa de shell. Viável para dev (`Dockerfile.dev`).
- **distroless** é o ponto ótimo para prod: mínima, segura, não-root, pronta para Go estático.

**Considerações**:
- **`.dockerignore`** OBRIGATÓRIO: excluir `.git`, `.env`, `.env.*`, `*.md`, `.mesa/`, binários locais, `vendor/` (se gerenciado), diretórios de IDE.
- **BuildKit** (`# syntax=docker/dockerfile:1`) habilita `--mount=type=cache,target=/go/pkg/mod` e `--mount=type=cache,target=/root/.cache/go-build` — reduz drasticamente o tempo de build em CI (~70% em builds incrementais).
- **`.devcontainer`** (opcional): facilita onboarding de novos devs no VSCode/Codespaces.

### 2.2 docker-compose.yml (dev/teste)

**Serviços essenciais**:

1. **app** (desenvolvimento):
   - Builda de `Dockerfile.dev` (variant com `air` para hot reload — Go não tem watch nativo).
   - Volume bind do código-fonte: `./:/app` (sincroniza alterações).
   - Porta exposta: `8080:8080`.
   - Depende de `db` com `condition: service_healthy`.

2. **db** (PostgreSQL):
   - Imagem pinada por SHA: `postgres:16-alpine@sha256:...`.
   - Volume nomeado `pgdata` para persistência entre `docker compose down/up` (não `down -v`).
   - Healthcheck: `pg_isready -U $POSTGRES_USER -d $POSTGRES_DB`.
   - Variáveis via `.env` (gitignored): `POSTGRES_PASSWORD`, `POSTGRES_USER`, `POSTGRES_DB`.
   - (Opcional) Porta exposta `5432:5432` para acesso de tools locais (DBeaver, psql) em dev.

3. **(Opcional) Redis**: para o MVP **não é necessário** se usarmos 1 réplica em dev. Recomendo deixar o serviço **comentado** no compose para acelerar a transição futura (ver Seção 9).

**Healthchecks**:
- `app` healthcheck é complicado com distroless (não tem wget/curl). Alternativas:
  - Usar `Dockerfile.dev` com `alpine` (tem wget) apenas para dev.
  - Compilar um binário Go `healthcheck` minúsculo (padrão recomendado pela comunidade Kubernetes).
  - Pular healthcheck no compose e depender do K8s readiness em prod (aceitável para dev local).

**Dev loop recomendado**:
- Hot reload via `air` (`github.com/cosmtrek/air`) dentro do container dev — recarrega o servidor Beego em mudança de arquivo.
- Migrations rodam no startup do app em dev (via init-container lógico dentro do entrypoint). Ver Seção 5.

### 2.3 Secrets em dev

- `.env` na raiz, **gitignored**. `.env.example` versionado como documentação.
- Variáveis mínimas: `POSTGRES_PASSWORD`, `DATABASE_URL`, `JWT_SECRET`, `ENCRYPTION_KEY` (master key para cifrar chaves de provedores no DB), `LOG_LEVEL=debug`.
- **Nunca** versionar `.env` real — pré-commit hook `detect-secrets` ou `gitleaks` como guard rail.

---

## 3. Helm Chart (Kubernetes produção)

### 3.1 Estrutura do chart

```
charts/ai-provider-pool/
├── Chart.yaml                 # apiVersion: v2, name, version, appVersion
├── values.yaml                # defaults sensatos (overlay-able)
├── values-dev.yaml            # 1 réplica, recursos baixos, log debug
├── values-staging.yaml        # 2 réplicas, log info, DB managed
├── values-prod.yaml           # 2+ réplicas, HPA, PDB, log warn
├── README.md                  # docs de uso (install, upgrade, rollback)
├── templates/
│   ├── _helpers.tpl           # labels, selectors, naming
│   ├── deployment.yaml        # app
│   ├── hpa.yaml               # HorizontalPodAutoscaler
│   ├── pdb.yaml               # PodDisruptionBudget
│   ├── service.yaml
│   ├── ingress.yaml
│   ├── configmap.yaml         # config não-sensível
│   ├── externalsecret.yaml    # ESO → materializa o Secret
│   ├── serviceaccount.yaml
│   ├── networkpolicy.yaml     # blast radius containment
│   └── NOTES.txt              # msg pós-install
```

### 3.2 Deployment

**Configurações críticas (auto-check conforme constraints do role DevOps)**:

- `replicas: 2` (mínimo para HA em prod). Em dev: `replicas: 1`.
- `strategy: RollingUpdate` com `maxSurge: 1`, `maxUnavailable: 0` — garante zero pods indisponíveis durante deploy. Importante porque graceful shutdown está no ROADMAP (ver 3.3).
- `revisionHistoryLimit: 10` — mantém histórico para rollback com `helm rollback`.
- `terminationGracePeriodSeconds: 60` — alto, para dar tempo de concluir streams em curso (mitigação parcial até graceful shutdown chegar).
- **Pod Security Context** (obrigatório):
  ```yaml
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532      # UID do distroless nonroot
    runAsGroup: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  ```
- **Container Security Context** (obrigatório):
  ```yaml
  securityContext:
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities:
      drop: ["ALL"]
  ```
- **Resources** (obrigatório):
  ```yaml
  resources:
    requests: { cpu: 100m, memory: 128Mi }
    limits:   { cpu: 500m, memory: 512Mi }
  ```
  Calibrar com profiling real em staging.
- `readOnlyRootFilesystem: true` requer volume writable para `/tmp` caso o Go/Beego grave arquivos temporários — montar `emptyDir` em `/tmp`.
- `topologySpreadConstraints` ou `podAntiAffinity` para distribuir réplicas entre AZs/nodes.

### 3.3 Probes (graceful shutdown no roadmap — probes no MVP)

- **startupProbe**: essencial para apps Go/Beego com migrations no init-container e warm-up. `failureThreshold: 30`, `periodSeconds: 10` → 5 min de tolerância antes de restart.
- **livenessProbe**: `/healthz` — processo vivo, **sem checar dependências** (evita restarts em cascata). `periodSeconds: 10`, `failureThreshold: 3`, `timeoutSeconds: 2`.
- **readinessProbe**: `/readyz` — checa DB + runtime readiness. Em falha, remove o pod do Service (não mata). `periodSeconds: 5`, `failureThreshold: 3`, `timeoutSeconds: 3`.

⚠️ **Sinal forte para o backend architect**: graceful shutdown está no ROADMAP, mas sem ele deploys RollingUpdate cortam streams SSE em andamento — para um produto de chat com streaming, isso é **visível para o usuário** (chat para de responder no meio). **Recomendo puxar graceful shutdown para o MVP ou para "MVP+1" imediato**. Mitigação até lá: `terminationGracePeriodSeconds: 60` + retry no cliente.

### 3.4 Service + Ingress

- **Service**: `ClusterIP`, porta `8080`, selector via labels padrão.
- **Ingress** (nginx-ingress recomendado pela flexibilidade de annotations):
  - TLS termination com cert-manager + Let's Encrypt (ou CA interno).
  - Annotations críticas para streaming:
    ```yaml
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"   # 1h
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-buffering: "off"        # ESSENCIAL para SSE
    nginx.ingress.kubernetes.io/configuration-snippet: |
      proxy_http_version 1.1;
      proxy_set_header Connection "";
    ```
  - `proxy-buffering: off` é **mandatório** — sem ele, nginx bufferiza respostas e SSE/streaming quebra.
  - HSTS, rate limit no ingress (primeira linha de defesa), CORS se cliente for browser (dashboard).
  - Path routing: API em `/`, dashboard em `/dashboard` (ou subdomínio `admin.api-pool.exemplo.com`).

### 3.5 ConfigMap + Secret

- **ConfigMap** (não-sensível): porta, log level, DB host, DB name, feature flags, default rate limit, max retries, backoff params, paths de config Beego, lista de provedores suportados.
- **Secret** (sensível): `DATABASE_URL` (ou `DB_PASSWORD`), `JWT_SECRET`, `ENCRYPTION_KEY` (master key para cifrar chaves de provedores no DB).

⚠️ **DISTINÇÃO CRÍTICA — Provider keys NÃO são secrets de infra**:

Esta é uma decisão arquitetural importante que afeta pipeline, secrets management e governança:

- As **chaves de API dos provedores** (Gemini, Mistral, DeepSeek, Z.ai, Kimi) ficam **cifradas no PostgreSQL** — não em variáveis de ambiente, não em Helm values, não em pipeline.
- Cifradas em repouso com AES-256-GCM usando `ENCRYPTION_KEY` (master key) que vem do Secret (via ESO/Sealed Secrets).
- O sistema busca e decifra as chaves do DB em runtime.
- **Justificativa**: 
  - As chaves são **dados de domínio** (cadastro CRUD pelo admin), não config de infra. Mudam com frequência (admin adiciona/remove/rotaciona).
  - Mantê-las em env vars exigiria redeploy a cada mudança — anti-pattern.
  - Já entrega parte do requisito de governança: chaves nunca passam por pipeline, nunca em Git.
- **Atenção ao backend architect**: implementar **criptografia envelope** (DEK por chave + KEK master), com rotação do KEK suportada — prepara para ROADMAP de compliance.

### 3.6 Gerenciamento de secrets — recomendação por maturidade

| Nível | Solução | Quando usar |
|-------|---------|-------------|
| 0 (proibido) | `Secret` Helm base64 nativo | Nunca em Git |
| 1 (simples) | **Sealed Secrets** (Bitnami) | Setup pequeno, GitOps-friendly, sem rotação automática |
| 2 (recomendado) | **External Secrets Operator (ESO)** + AWS Secrets Manager / GCP Secret Manager / Azure Key Vault | Setup prod real, com rotação, auditoria, IRSA/WIF |
| 3 (enterprise) | **HashiCorp Vault** + ESO ou injetor direto | Multi-cluster, multi-tenancy, política complexa |

**Recomendação para este projeto**: **ESO + backend gerenciado do cloud provider** onde o cluster roda.

Justificativa:
- Equipe pequena, mas o produto tem requisitos de governança (a dor original era compartilhamento inseguro de chaves — não faz sentido reforçar a infra com práticas fracas).
- ESO é declarativo (GitOps-friendly), suporta rotação, integra com identidade cloud sem long-lived creds (IRSA na AWS, Workload Identity no GCP).
- **Migration path**: Sealed Secrets → ESO é direta; a transição ESO → Vault também. Começar com ESO não bloqueia nada.

**Anti-padrões explícitos a evitar**:
- Long-lived access keys do cloud provider no GitHub Actions secrets → usar **OIDC federation**.
- Secrets base64 no `values.yaml` → sempre ExternalSecret.
- Bakear secrets em camadas de imagem Docker → irreversível (histórico de layers persiste).

---

## 4. Configuração por ambiente

### 4.1 Estratégia (12-factor)

- **Variáveis de ambiente como fonte primária** — Beego suporta `${ENV_VAR}` em `conf/app.conf` e `os.Getenv` em código.
- **Hierarquia de precedência**: defaults em código → `conf/app.conf` → env vars → flags CLI (env sempre vence).
- **Nunca** conflito entre arquivo e env: env é sempre overridable.
- **Schema validation**: usar biblioteca tipo `envconfig` (Go) para validar tipos e required vars no startup — falha rápido em config errada.

### 4.2 Separação dev/staging/prod

- `values-dev.yaml`: 1 réplica, recursos baixos, log `debug`, DB embedded no compose, HPA desativado, degradações "amigáveis".
- `values-staging.yaml`: 2 réplicas, log `info`, DB managed (réplica lógica de staging do prod), HPA mínimo, mirror do prod para testes realistas.
- `values-prod.yaml`: 2+ réplicas, log `info`/`warn`, managed DB com backup + PITR, HPA + PDB obrigatórios, network policies estritas.

### 4.3 Anti-padrões a evitar
- Versionar `.env` com segredos.
- Bakear segredos no Dockerfile (camadas expostas mesmo em multi-stage).
- Usar `latest` em image tags — sempre SHA digest (imutável) ou semver (versão publicada).
- Hardcoded values em código (DB host, etc).

---

## 5. Banco de dados em produção

### 5.1 Decisão: Managed DB vs StatefulSet self-hosted

**Recomendação: Managed DB** (CloudSQL / RDS / Azure Database for PostgreSQL) para produção.

Justificativa:
- Equipe é pequena (escala de ~30 usuários internos), o custo operacional de manter PG StatefulSet (backups, replicas, upgrades, tuning, failover, monitoramento de WAL, vacuum, etc.) é desproporcional ao benefício.
- Managed DB oferece: backups automáticos point-in-time, PITR, HA configurável (multi-AZ), patches gerenciados, métricas prontas, scaling vertical simples, failover automático.
- **Exceção**: se houver requisito de soberania de dados / on-prem / custo cloud proibitivo, self-host via **CloudNativePG** (recomendado pela CNCF, moderno) ou **Zalando Postgres Operator** (maduro). Nunca StatefulSet PG cru + script bash.

**Para dev/teste**: PostgreSQL no docker-compose (sem HA), volume nomeado. Suficiente.

### 5.2 Conexão do app ao DB

- Secret com `DATABASE_URL=postgres://user:pass@host:5432/dbname?sslmode=verify-full&sslrootcert=/etc/ssl/certs/ca.pem`.
- **SSL obrigatório em prod**: `sslmode=verify-full` com CA bundle do managed DB montado como ConfigMap/Secret.
- **Connection pooling** no app: usar `pgxpool` (não `database/sql` puro) com max conns calibrado (ex.: 25), max idle (ex.: 5), timeout. Crítico para não esgotar conexões quando escalar réplicas.
- (Opcional) **PgBouncer** sidecar ou gerenciado (CloudSQL tem Auth Proxy + connection pooling) se o número de réplicas crescer.

### 5.3 Migrations — automação

**Recomendação: Job/init-container no deploy, não manual**.

Padrão preferido (produção multi-réplica):
- **Helm pre-install/pre-upgrade Hook** que roda um **Job** executando `./app migrate up`.
- Vantagem: roda **uma vez** antes do Deployment subir, evitando N réplicas competindo por migrations.
- O Job deve ter `restartPolicy: OnFailure` e um timeout razoável.

Alternativa aceitável (dev/single-réplica):
- **Init container** no Deployment que roda migrations antes do app principal. Mais simples, mas se múltiplas réplicas sobem juntas, todas rodam migrations — precisa de migration tool que suporte lock (advisory lock no PG).

**Tool recomendada**: **golang-migrate** (`github.com/golang-migrate/migrate/v4`) ou **goose** (`github.com/pressly/goose/v3`). Ambos:
- Versionam migrations em arquivos numerados (`0001_init.up.sql` / `0001_init.down.sql`).
- Suportam lock distribuído (advisory lock no PG).
- Idempotentes.
- Suportam up/down.

⚠️ **Anti-pattern explícito**: ORM auto-migrate (Beego `RunSyncdb` ou GORM `AutoMigrate`) em produção — não versiona, não dá rollback, não tem lock, perde dados em alterações de schema. Aceitável apenas em dev para bootstrap inicial.

### 5.4 Backup automatizado e DR

- **Managed DB**: backups automáticos diários + PITR (point-in-time recovery, retention configurável — ex.: 7-30 dias). Habilitar desde o dia 1.
- **Self-hosted**: CronJob no K8s que roda `pg_dump` → upload para S3/GCS com retention policy (ex.: 30 dias diário + 12 mensais).
- **Restore drill**: documentar procedimento no runbook e **testar mensalmente** em ambiente isolado (constraint do role: "every deployment procedure is reproducible by any team member").

---

## 6. CI/CD

### 6.1 Pipeline recomendado (GitHub Actions)

Estágios (todos via **reusable workflows** para reaproveitar entre PR/main/release):

1. **lint** (PR + main): `golangci-lint run`, `go vet ./...`, `go fmt -l .` (falha se houver diff).
2. **test** (PR + main): `go test -race -cover -coverprofile=coverage.out ./...`. Cobertura mínima aceitável: configurar (ex.: 70%).
3. **security-scan** (PR + main): 
   - `gosec ./...` (SAST Go).
   - `govulncheck ./...` (deps Go).
   - `trivy fs --severity HIGH,CRITICAL .` (filesystem + IaC).
   - Falha em HIGH/CRITICAL.
4. **build-image** (main + tags): build multi-stage Dockerfile, push para registry.
   - Tag = `${{ github.sha }}` (imutável, sempre).
   - Tag adicional = `${{ github.ref_name }}` se for tag semver (`v1.2.3`).
   - **Proibido**: tag `latest`, `main`, branch name (mutáveis).
5. **scan-image** (após push): `trivy image --severity CRITICAL <ref>`. Falha em CRITICAL.
6. **deploy-staging** (push to main): `helm upgrade --install ai-provider-pool --atomic --timeout 5m -f values-staging.yaml --set image.tag=${{ github.sha }}`.
7. **deploy-prod** (manual approval via `environment: production`): mesmo comando com `values-prod.yaml` + smoke test pós-deploy (curl `/healthz`).

**Padrões obrigatórios**:
- **OIDC federation** para autenticar com cloud (AWS/GCP/Azure) — sem long-lived access keys no GitHub.
- **Concurrency groups** por ambiente: `group: deploy-staging` impede deploy paralelo para mesmo env.
- **Cache**: `actions/setup-go` com `cache: true` para módulos Go + `docker/build-push-action` com BuildKit cache via GHA cache.
- **Secret scanning** nativo do GitHub + `gitleaks` em pre-commit hook.
- **Dependabot** habilitado para Go modules, GitHub Actions, Docker base images.

### 6.2 Registry + tags + supply chain

- **Registry**: GHCR (mais simples para GitHub, grátis para público), ECR (AWS), GAR (GCP). Para produto MIT open source, GHCR é ótimo padrão.
- **Tags**:
  - `main` branch → SHA digest (imutável).
  - Tags `vX.Y.Z` → `X.Y.Z` (semver), também SHA digest.
  - **Proibido**: `latest`, branch names (mutable).
- **Supply chain security** (pós-MVP, mas preparar): **Cosign** para assinar imagens + **SBOM** (`syft`) gerado no build. Adicionar `cosign verify` como admission policy no cluster (Kyverno/OPA) no roadmap.

### 6.3 GitOps vs Push-based

**Recomendação para MVP: Push-based** (GitHub Actions → `helm upgrade`).

Justificativa:
- Time pequeno, simplicidade vence.
- GitOps (ArgoCD/Flux) adiciona componentes no cluster, RBAC, sync waves, learning curve, maintenance overhead.
- Push-based com `helm upgrade --atomic` já dá rollback automático em falha de deploy.

**Migration path** (quando fizer sentido):
- Múltiplos ambientes + múltiplas equipes + drift detection → migrar para **ArgoCD** com uma `Application` por ambiente.
- Chart já é GitOps-ready (estado declarativo, values versionados, sem steps imperativos).
- Para adotar GitOps: pasta `manifests/<env>/` com `helm template` renderizado ou `HelmRelease` (Flux).

---

## 7. Observabilidade

### 7.1 Logging

- **JSON structured logging desde o MVP**. Biblioteca: `log/slog` (stdlib Go 1.21+, recomendado por não adicionar dep) ou `zerolog`/`zap` (mais performante).
- **Campos padrão**: `timestamp`, `level`, `msg`, `request_id`, `trace_id`, `user_id`, `provider`, `key_id` (hash, nunca chave), `model`, `tokens_in`, `tokens_out`, `latency_ms`, `status`, `error_type`.
- Em K8s: **stdout/stderr** (nunca arquivos) → coletado por **Loki** (com Fluent-bit/Promtail) ou cloud logging nativo (Cloud Logging, CloudWatch).
- **NUNCA** logar payloads de request/response (PII, conteúdo sensível, possíveis chaves vazadas em erro) — apenas metadados.
- **Audit log** (briefing pede item 9): tabela dedicada no DB **+** log JSON estruturado. Distinção: audit é estruturado para relatório/governança/regulatório; log é para ops/troubleshooting.

### 7.2 Tracing

- **OpenTelemetry** SDK no app **desde o MVP** — mesmo sem collector em prod no dia 1, instrumentation agora é barata e essencial depois.
- Trace propagation: cliente → proxy → provedor (se o provedor suportar headers W3C Trace Context — maioria não suporta, mas propagation interna sim).
- Distribuir um `trace_id` por request, propagar entre retries/failover para correlacionar no painel.
- Para identificar gargalos: span por tentativa de chave (útil para visualizar o failover em ação).

### 7.3 Metrics (ROADMAP — preparar o caminho)

- Endpoint `/metrics` Prometheus está no ROADMAP. 
- **Preparar desde já**: biblioteca `prometheus/client_golang` no código, expondo:
  - Contador: `ai_provider_pool_requests_total{provider, model, status}`.
  - Histograma: `ai_provider_pool_request_duration_seconds`.
  - Contador: `ai_provider_pool_failover_total{provider}`.
  - Gauge: `ai_provider_pool_keys_disabled_total{provider}`.
  - Gauge: `ai_provider_pool_keys_active{provider}`.
- `/metrics` pode ficar **desativado no MVP** (env var `METRICS_ENABLED=false`, não exposto no Service) e ligar depois — mas **a instrumentation existe desde o início** (sem rework).
- Recording rules + alerting rules virão depois, mas o chart já pode incluir placeholder.

### 7.4 Alerting (mínimo recomendado para prod)

| Alerta | Severidade | Runbook |
|--------|-----------|---------|
| Pod down / `kube_pod_status_phase != Running` | Page | Restart investigação |
| Alta taxa de 5xx (>5% por 5min) | Page | Ver logs, DB, provedores |
| Falha de conexão com DB | Page | Verificar managed DB, network |
| Todas as chaves de um provedor desabilitadas | Page | Verificar status do provedor, reativar manual |
| Disco/DB storage > 80% | Warn | Provisionar mais storage |
| Latência p99 > 5s | Warn | Investigar provedor |

**Cada alerta deve ter runbook correspondente** (constraint do role DevOps: "Every alert has a corresponding runbook"). Documentar em `docs/runbooks/`.

---

## 8. Health checks & Networking

### 8.1 Endpoints

- `/healthz`: processo vivo. `{"status":"ok"}`. Não checa dependências. Resposta 200.
- `/readyz`: checa DB + (opcional) cache connection. `{"status":"ready","checks":{"db":"ok"}}`. 503 se não pronto.
- `/livez` (alias opcional): mesma coisa que `/healthz`.
- `/version`: `{"version":"X.Y.Z","commit":"<sha>","built_at":"<iso>"}`. Útil para debug e para checar rollout.

### 8.2 Timeouts

- **Servidor Beego**:
  - `ReadTimeout: 30s` (tempo para cliente enviar request completa).
  - `WriteTimeout: 0` (sem timeout, ou altíssimo como 1h) — **mandatório** para streaming de chat, senão Beego corta streams longos.
  - `IdleTimeout: 120s`.
- **HTTP client para provedores**:
  - `Timeout: 0` (sem timeout no client) — usar `context.WithTimeout` ou `context.WithDeadline` para cancelamento controlado por request.
  - Dial timeout, TLS handshake timeout, response header timeout: configurar (ex.: 10s cada).
- **Ingress**: timeouts altos (ver Seção 3.4).

### 8.3 TLS

- **TLS termination no Ingress** com cert-manager + Let's Encrypt (prod) ou CA interno (corp).
- Interno no cluster: plain HTTP entre Ingress e Service (aceitável para MVP, rede é trusted dentro do namespace).
- **mTLS interno** via Service Mesh (Istio/Linkerd) — fora do escopo MVP, mencionar como roadmap.

### 8.4 Network policies (blast radius containment)

- Obrigatório em prod (constraint do role: "Design for blast radius containment"):
- **Ingress** ao namespace do app: apenas do Ingress controller namespace.
- **Egress** do app: apenas para DB (porta 5432), provedores LLM (HTTPS 443, opcionalmente via egress proxy/controlado), DNS (UDP 53), e OIDC endpoints.
- Negar tudo por default (`DenyAll`), liberar explicitamente.
- Justificativa: se o pod for comprometido, limite o blast radius — sem acesso a outros namespaces, sem exfiltração para destinos não autorizados.

---

## 9. Escalabilidade horizontal — O PONTO MAIS CRÍTICO

⚠️ **Este é o maior risco arquitetural do MVP** e o briefing não endereça explicitamente. Preciso chamar atenção dos pares.

### 9.1 O proxy NÃO é stateless no MVP

Estado em memória por pod identificado:

1. **LRU das chaves** — qual chave usar a seguir (Least Recently Used). Em memória = cada pod tem visão parcial do ranking global.
2. **Rate limiting** por usuário/token (req/min). Se em memória, cada pod conta independentemente — **limite real efetivo = `limite × N réplicas`** (rate limit ignorado).
3. **Afinidade de cache de contexto** — último (user, chave, msg) por usuário em memória. Por pod, fragmentado.
4. **Circuit breaker / backoff** por chave — estado de "chave em cooldown" também é por pod (cada pod redescobre a chave falhando).

**Impacto em múltiplas réplicas**:

| Estado | Problema com multi-réplica | Severidade |
|--------|---------------------------|------------|
| LRU | Distribuição desigual (subótima, não bloqueante) | Médio |
| Rate limit | **Limite real = limite × N**, falha de governança | **ALTO/Bloqueante** |
| Context affinity | Fragmentado, perde benefício do cache do provedor | Médio |
| Circuit breaker | Cada pod redescobre chave falhando (custo, mas recupera) | Baixo-Médio |

### 9.2 Opções

**Opção A — Réplica única (MVP simplório)**
- Mais simples. Sem HA — uma queda = downtime durante restart.
- Aceitável para uso interno de ~30 usuários em dev/staging? Sim. Em prod? **Arriscado**.
- Mitigação parcial: `strategy: Recreate` durante deploy (mas com downtime curto, anti-HA).

**Opção B — Redis para estado compartilhado (recomendado para multi-réplica)**
- Adiciona Redis como dependência.
- LRU: Redis sorted set com timestamp do último uso por chave (`ZADD keys_lru <ts> <key_id>`).
- Rate limit: Redis com `INCR` + `EXPIRE` (sliding window ou fixed window por usuário/token).
- Context affinity: Redis hash com TTL por usuário (`HSET last_affinity <user_id> <key_id>`, `EXPIRE` 1h).
- Circuit breaker: Redis key por chave com TTL (`SET cb:<key_id> 1 EX 60`).
- Trade-off: latência extra (~0.5-1ms por op), mas habilita escala real e consistência entre pods.
- **Recomendação**: incluir Redis no MVP SE o objetivo é multi-réplica em prod. Para 30 usuários internos, mesmo assim é discutível — mas prepara o caminho para tudo no ROADMAP (times, quotas).

**Opção C — Sticky sessions via Ingress**
- Cookie-based session affinity (`nginx.ingress.kubernetes.io/affinity: cookie`).
- Garante que um mesmo user sempre vai para o mesmo pod (rate limit e affinity funcionam localmente).
- **Problemas**:
  - Streaming/SSE + sticky é frágil (long-lived connections sobrecarregam um pod).
  - Balanceamento desigual (hot spots).
  - Perde affinity se pod cai (user recomeça do zero).
  - Falha no deploy rolling (cookie ainda aponta para pod terminado).
- **Não recomendado** como solução primária — só como mitigação temporária.

### 9.3 Recomendação DevOps

**Para o MVP**:

1. **Implementar abstração de store** desde o início (interface Go):
   ```go
   type Store interface {
       GetLastUsedKey(provID string) (keyID string, err error)
       MarkKeyUsed(provID, keyID string) error
       IncrRateLimit(userID string, window time.Duration) (count int, err error)
       GetContextAffinity(userID string) (keyID string, err error)
       SetContextAffinity(userID, keyID string, ttl time.Duration) error
       IsCircuitOpen(keyID string) (bool, error)
       OpenCircuit(keyID string, ttl time.Duration) error
   }
   ```
2. **Duas implementações**:
   - `memoryStore` (default, em dev e single-réplica).
   - `redisStore` (ativado por env `STORE_TYPE=redis`).
3. **Em prod**: 2 réplicas + Redis no chart → HA + escala correta + consistência.
4. **Em dev**: 1 réplica sem Redis (memoryStore) → simplicidade.

Isso resolve o problema E prepara para o futuro (multi-tenancy: namespace Redis por tenant; quotas: contador distribuído; etc.) **sem rework**.

### 9.4 HPA + PDB

- **HPA** (HorizontalPodAutoscaler):
  - Baseado em CPU (target 60%) e/ou custom metric (requests em flight — via Prometheus adapter quando `/metrics` chegar).
  - Min `2`, max `5` (para 30 usuários internos, raramente precisará escalar além; max alto seria desperdício).
  - Scale down stabilization window: 5min (não flapping).
- **PDB** (PodDisruptionBudget):
  - `minAvailable: 1` (sempre 1 pod disponível durante manutenção voluntária — node drain, upgrade).
  - Crítico para HA real.

---

## 10. Riscos e Considerações Finais

### 10.1 Riscos técnicos priorizados

| # | Risco | Severidade | Mitigação |
|---|-------|-----------|-----------|
| 1 | **Estado em memória + multi-réplica** (Seção 9) | ALTO | Store abstraction (memory + Redis) |
| 2 | **Streaming + graceful shutdown** | MÉDIO-ALTO | Puxar graceful shutdown para MVP+1; `terminationGracePeriodSeconds: 60` enquanto isso |
| 3 | **Beego less mainstream** (vs gin/echo/chi) | MÉDIO | Decisão do briefing, aceita. Documentar gotchas |
| 4 | **Parsing de mensagens de erro** frágil por provedor | MÉDIO | Isolar em adapter pattern, um parser por provedor, testes por provedor |
| 5 | **Failover cross-provider** (ROADMAP) — diferença de context window | BAIXO (roadmap) | Schema flexível para modelos desde o início |
| 6 | **DB connection esgotamento** com réplica | BAIXO | pgxpool com limites, PgBouncer se escalar |

### 10.2 Priorização DevOps (MVP)

**Must (bloqueante para deploy em prod)**:
- ✅ Multi-stage Dockerfile distroless + `.dockerignore` + BuildKit cache.
- ✅ docker-compose com app + PostgreSQL + volume + healthchecks.
- ✅ Helm chart: Deployment, Probes (startup/liveness/readiness), Service, Ingress com annotations de streaming, ConfigMap, ExternalSecret.
- ✅ Managed DB com backup + PITR habilitado.
- ✅ Migrations via Helm hook Job com golang-migrate.
- ✅ CI/CD pipeline completo (lint → test → scan → build → scan-image → deploy staging + manual prod) com OIDC.
- ✅ Health endpoints `/healthz`, `/readyz`, `/version`.
- ✅ JSON structured logging.
- ✅ **Store abstraction** (in-memory default, Redis-ready) — VOTAR COM PARES.
- ✅ Network policies (DenyAll default + liberações explícitas).
- ✅ Resource requests/limits + securityContext (runAsNonRoot, readOnlyRootFS, drop ALL caps).
- ✅ PDB + HPA.

**Should (MVP+)**:
- 🟡 Redis no chart para multi-réplica consistente.
- 🟡 **Graceful shutdown** (puxar do ROADMAP para MVP+1 — impacto em UX é visível).
- 🟡 OpenTelemetry tracing instrumentation.
- 🟡 Prometheus instrumentation (mesmo sem endpoint exposto).
- 🟡 Kyverno policies de admission (enforce runAsNonRoot, no hostNetwork, etc).

**Could (roadmap pós-MVP)**:
- 🟢 GitOps (ArgoCD) quando houver múltiplos ambientes + múltiplas equipes.
- 🟢 Cosign signing + SBOM.
- 🟢 Chaos testing (Chaos Mesh).
- 🟢 Service mesh (Linkerd/Istio) para mTLS interno.
- 🟢 KEDA para event-driven scaling (escala por requests em flight).
- 🟢 PgBouncer se o número de réplicas crescer.

### 10.3 Preparação para ROADMAP

Decisões agora que **pavimentam o futuro** sem rework:

- **Store abstraction** (Seção 9.3) → prepara multi-tenancy (cada tenant com namespace Redis próprio), quotas distribuídas, rate limit cross-pod.
- **Provider adapter pattern** → adiciona failover cross-provider fácil (ROADMAP), modelos "auto" com roteamento heurístico.
- **`/metrics` instrumented but disabled** → liga em 1 commit quando Prometheus chegar (sem refatoração).
- **OpenTelemetry tracing instrumentation** desde o MVP → collector no futuro = plug-and-play.
- **Helm chart com values overlays** (`values-<env>.yaml`) → fácil de replicar para times diferentes quando multi-tenancy chegar.
- **Schema de DB com `tenant_id` nullable desde o início** → decisão para o backend architect, mas alinha com DevOps (zero-downtime migration futura).
- **Encryption envelope (DEK/KEK)** para chaves de provedores no DB → rotação futura de master key sem recriptografar tudo.

### 10.4 Open questions para discutir com pares

1. **Backend Architect**: Confirma a necessidade de store abstraction? Redis entra no MVP ou MVP+1? Impacto no rate limit e context affinity é bloqueante para multi-réplica em prod?
2. **Backend Architect**: Graceful shutdown — concorda em puxar para o MVP dado o impacto em streaming?
3. **Backend Architect**: Schema de DB com `tenant_id` nullable desde o início — concorda para preparar multi-tenancy sem migration cara?
4. **Security Specialist**: Confirma estratégia de **criptografia envelope** para chaves de provedores no DB + ESO para master key? Rotação de KEK no MVP ou roadmap?
5. **Security Specialist**: Network policies DenyAll default — algum ajuste necessário para provedores externos (egress para IPs dinâmicos)?
6. **Todos**: Managed DB vs self-hosted StatefulSet — confirmação considerando custos vs benefícios para 30 usuários internos?

---

## Resumo DevOps

A infraestrutura é factível com stack convencional e madura: **Docker multi-stage distroless + Helm + managed PostgreSQL + GHCR + GitHub Actions push-based + ESO para secrets**. Os principais entregáveis são: Dockerfile otimizado, docker-compose com hot reload, Helm chart completo (Deployment/Probes/Service/Ingress/HPA/PDB/NetworkPolicy/ExternalSecret), pipeline CI/CD com security gates, e JSON logging estruturado. **O maior risco** é o estado em memória do proxy (LRU + rate limit + context affinity) que quebra em multi-réplica — recomendo fortemente **store abstraction (in-memory + Redis) já no MVP** para destravar HA corretamente e pavimentar o ROADMAP. Provider keys ficam **cifradas no DB** (criptografia envelope), não como secrets de infra. Graceful shutdown deve sair do ROADMAP e entrar no MVP+1 — sem ele, deploys RollingUpdate cortam streams de chat visivelmente.