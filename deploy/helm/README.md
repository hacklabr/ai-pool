# Helm Chart — AI Provider Pool (produção)

Helm chart para deploy do AI Provider Pool em Kubernetes. Especificação técnica
de referência: `/.mesa/specifications/spec-388d67e6.md` §3.5.2.

## Decisões arquiteturais (consolidadas pelos especialistas)

- **1 réplica** (Opção A — single-replica MVP). HPA/PDB omitidos (sem efeito com 1 pod).
- **RollingUpdate** `maxSurge:1 maxUnavailable:0` + graceful shutdown = zero-downtime.
- **Imagem `distroless/base-debian12:nonroot`** (UID 65532, CA roots p/ HTTPS).
- **Hardening**: `readOnlyRootFilesystem`, `runAsNonRoot`, `drop ALL caps`,
  `seccompProfile: RuntimeDefault`, `automountServiceAccountToken: false`.
- **PostgreSQL**: templates NATIVOS (padrão Hacklab, desde chart v0.2.0).
  Não usa mais subchart Bitnami. Imagem oficial `postgres:16-alpine`.
  StatefulSet com `volumeClaimTemplates`, senha auto-gerada persistente via
  `lookup`, `helm.sh/resource-policy: keep` (sobrevive a `helm uninstall`).
- **Migrations via Helm `post-install/post-upgrade` Job hook** rodando
  `aipp --migrate-only`. Tem init container `wait-for-db` (pg_isready loop)
  que espera o postgres ficar Ready antes de migrar.
- **Secrets**: External Secrets Operator (gold standard). KEK + token_pepper
  montados como **arquivos readOnly** (defesa em profundidade — não como env var).
  Para smoke test: Secret manual pré-criado (ver INSTALL.md).
- **Ingress** com annotations SSE críticas (`proxy-buffering: off`, timeouts 1h).
- **NetworkPolicy** DenyAll default + liberações explícitas (DB, DNS, HTTPS externo).
  Desativada no `values-prod.yaml` (cluster Hacklab de confiança interna).
- **Velero backup**: podAnnotations + extraMountPoints suportados no postgres
  pod. Hook `pre.hook.backup.velero.io` com `pg_dump` para emptyDir (consistência).

## Pré-requisitos no cluster

1. **Ingress controller** nginx (ou compatível que respeite annotations `nginx.ingress.kubernetes.io/*`).
2. **cert-manager** + **ClusterIssuer** `letsencrypt-prod` (para TLS via Let's Encrypt).
3. **External Secrets Operator** + **SecretStore/ClusterSecretStore** apontando
   para seu backend (Vault, AWS SM, GCP SM, Azure KV). Necessário se
   `externalSecret.enabled=true`. Alternativa: criar Secret manualmente.
4. **CNI com NetworkPolicy** (Calico, Cilium, AWS VPC CNI, etc.) — se
   `networkPolicy.enabled=true`.
5. **StorageClass** para o PVC do PostgreSQL (ex.: `ceph-blockstorage`,
   `gp3`, `premium-rwo`).
6. (Opcional) **Velero** com fs-backup para snapshots do volume do postgres.

## Instalação — passo a passo (produção)

> Cenário Hacklab: PostgreSQL nativo (imagem oficial), KEK criado manualmente,
> Ingress nginx + TLS Let's Encrypt, Velero backup via pre-hook `pg_dump`.
> Namespace alvo: `hacklab-ai-pool`. Release name: `hacklab-ai-pool`.

### Passo 1 — Gerar KEK + token pepper (uma única vez)

O proxy cifra as chaves de provedores com AES-256-GCM. A KEK e o pepper são
**gerados uma única vez** e devem ser **salvos em cofre offline imediatamente**
(perdeu = perdeu todas as chaves cifradas no banco).

```bash
KEK=$(openssl rand -base64 32)
PEPPER=$(openssl rand -base64 48)
echo "KEK=$KEK"
echo "PEPPER=$PEPPER"
# ⚠️  SALVE A SAÍDA EM COFRE OFFLINE AGORA (passphrase manager, vault físico)
```

### Passo 2 — Criar namespace + Secret com KEK/pepper

O nome do Secret **deriva do release name** (`<release>-secrets`). Com release
`hacklab-ai-pool`, o Secret se chama `hacklab-ai-pool-secrets`.

```bash
kubectl create namespace hacklab-ai-pool

kubectl -n hacklab-ai-pool create secret generic hacklab-ai-pool-secrets \
  --from-literal=kek="$KEK" \
  --from-literal=token_pepper="$PEPPER"
```

> Em produção real, prefira External Secrets Operator (`externalSecret.enabled=true`)
> materializando este Secret a partir do seu vault — evita secret no shell history.

### Passo 3 — Helm install

```bash
helm install hacklab-ai-pool deploy/helm/ai-pool \
  -n hacklab-ai-pool \
  -f deploy/helm/ai-pool/values-prod.yaml
```

O `helm install` imprime o `NOTES.txt` com smoke tests. O que sobe:

- **PostgreSQL StatefulSet** (`postgres:16-alpine`, PVC `ceph-blockstorage`,
  senha auto-gerada persistente).
- **Deployment** da app (1 réplica, graceful shutdown, RollingUpdate zero-downtime).
- **Migration Job** (`post-install` hook — init container `wait-for-db` espera
  o postgres Ready, depois roda `aipp --migrate-only`).
- Service, Ingress (nginx + cert-manager + annotations SSE), ConfigMap,
  ServiceAccount, Secret do DB.

### Passo 4 — Verificar pós-install

```bash
# Job de migrations completou? (pode levar 1-2 min na 1ª instalação)
kubectl -n hacklab-ai-pool logs job/hacklab-ai-pool-migrate --tail=-1 | tail -10
# Look for: "migrations applied successfully"

# Pods prontos?
kubectl -n hacklab-ai-pool get pods
# Expect: hacklab-ai-pool-postgresql-0 Running
#         hacklab-ai-pool-xxxxx Running
#         hacklab-ai-pool-migrate-xxxx Completed

# TLS emitido pelo cert-manager? (~1 min)
kubectl -n hacklab-ai-pool get certificate
# Expect: ai-pool-tls True

# Smoke test HTTP
kubectl -n hacklab-ai-pool port-forward svc/hacklab-ai-pool 8080:80 &
curl -s http://localhost:8080/healthz    # {"status":"ok"}
curl -s http://localhost:8080/readyz     # {"status":"ready",...}

# Ou via Ingress (após DNS + TLS prontos):
curl -s https://ia.hacklab.com.br/healthz
```

### Passo 5 — Bootstrap do admin (uma única vez)

Em `DEV_MODE=false` (produção), o admin não é criado automaticamente.
Crie via CLI do próprio binário (senha ≥ 12 chars):

```bash
kubectl -n hacklab-ai-pool exec deploy/hacklab-ai-pool -- \
  env AIPP_BOOTSTRAP_ADMIN_PASSWORD='SuaSenhaForte123' \
  /aipp --bootstrap-admin-username=admin
```

### Passo 6 — Configurar provedores via UI

Acesse `https://ia.hacklab.com.br/web/login` e cadastre:

1. **Provider** (ex.: Z.ai, DeepSeek, Gemini, Mistral, Kimi).
2. **Model** vinculado ao provider.
3. **Key** (a chave real do provedor — será cifrada com AES-256-GCM no DB).
4. **Token** `sk-aipp-...` para seus clientes usarem.

Teste o endpoint OpenAI-compatible:

```bash
curl https://ia.hacklab.com.br/v1/chat/completions \
  -H "Authorization: Bearer sk-aipp-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"Olá!"}]}'
```

## Operações

```bash
# Atualizar (após mudar values-prod.yaml ou nova tag de imagem)
helm upgrade hacklab-ai-pool deploy/helm/ai-pool \
  -n hacklab-ai-pool -f deploy/helm/ai-pool/values-prod.yaml

# Rollback
helm rollback hacklab-ai-pool -n hacklab-ai-pool

# Logs
kubectl -n hacklab-ai-pool logs -f deploy/hacklab-ai-pool
kubectl -n hacklab-ai-pool logs -f hacklab-ai-pool-postgresql-0

# Senha atual do postgres (auto-gerada)
kubectl -n hacklab-ai-pool get secret hacklab-ai-pool-db -o jsonpath='{.data.password}' | base64 -d

# Remover release (NÃO apaga PVC do postgres — helm.sh/resource-policy: keep)
helm uninstall hacklab-ai-pool -n hacklab-ai-pool

# Reset completo (cuidado: apaga dados)
kubectl -n hacklab-ai-pool delete statefulset hacklab-ai-pool-postgresql
kubectl -n hacklab-ai-pool delete pvc -l app.kubernetes.io/component=postgresql
```

## Rotação de senha do PostgreSQL

A senha é auto-gerada na 1ª instalação e persistida entre upgrades. Para rotacionar:

```bash
NEW_PASS=$(openssl rand -base64 24)
helm upgrade hacklab-ai-pool deploy/helm/ai-pool \
  -n hacklab-ai-pool -f deploy/helm/ai-pool/values-prod.yaml \
  --set postgresql.auth.password="$NEW_PASS"
# O Secret "<release>-db" é regenerado com nova senha + DATABASE_URL em sync.
# Reinicie o pod do postgres para aplicar:
kubectl -n hacklab-ai-pool delete pod hacklab-ai-pool-postgresql-0
```

## Outros cenários de instalação

### Smoke test (DATABASE_URL inline — NÃO usar em produção)

```bash
helm upgrade --install ai-pool deploy/helm/ai-pool \
  --namespace ai-pool --create-namespace \
  --set postgresql.enabled=false \
  --set database.url="postgres://user:pass@external-host:5432/aipp?sslmode=require"
```

### DB gerenciado (CloudSQL/RDS/Azure DB)

```bash
kubectl -n ai-pool create secret generic ai-pool-db \
  --from-literal=DATABASE_URL='postgres://USER:PASS@cloudsql:5432/aipp?sslmode=require'

helm install ai-pool deploy/helm/ai-pool \
  --namespace ai-pool --create-namespace \
  --set postgresql.enabled=false \
  --set database.existingSecret=ai-pool-db
```

## Estrutura

```
ai-pool/
├── Chart.yaml              # name=ai-pool, version=0.2.0, appVersion=0.1.0 (sem dependencies)
├── values.yaml             # defaults (postgres nativo, KEK via Secret, etc.)
├── values-prod.yaml        # produção Hacklab (ia.hacklab.com.br, ceph-block, LE, Velero)
└── templates/
    ├── _helpers.tpl            # nomes canonical, labels, helpers postgres
    ├── configmap.yaml          # config NÃO-sensível (HTTP_ADDR, DEV_MODE, LOG_*, ...)
    ├── secret.yaml             # DATABASE_URL inline (smoke); ou no-op quando PG embutido
    ├── externalsecret.yaml     # ESO → Secret K8s (KEK + pepper + DATABASE_URL)
    ├── deployment.yaml         # RollingUpdate, probes, securityContext, volumes
    ├── service.yaml            # ClusterIP porta 80 → targetPort 8080
    ├── ingress.yaml            # nginx-ingress com annotations SSE + cert-manager
    ├── migration-job.yaml      # Helm hook post-install/post-upgrade (aipp --migrate-only)
    ├── networkpolicy.yaml      # DenyAll + egress DB/DNS/HTTPS (opcional)
    ├── serviceaccount.yaml     # SA sem RBAC (app não fala com K8s API)
    ├── postgresql-statefulset.yaml  # PG nativo: StatefulSet + volumeClaimTemplates + Velero ann
    ├── postgresql-service.yaml      # Service ClusterIP para o PG
    ├── postgresql-secret.yaml       # Secret com password (auto-gerada) + DATABASE_URL
    └── NOTES.txt               # instruções pós-install (smoke tests, bootstrap admin)
```

## Validação local (sem cluster)

```bash
make helm-lint                                              # valida
make helm-template HELM_SETS="--set ingress.enabled=true"   # renderiza
```

## Migrations — como funciona

1. O binário `aipp` embarca as migrations via `//go:embed` (migrations/embed.go).
2. Helm cria um **Job** (`post-install,post-upgrade` hook) que roda `aipp --migrate-only`.
3. O Job tem um **init container** `wait-for-db` (imagem `postgres:16-alpine`) que
   faz loop `pg_isready` até o PostgreSQL responder. Necessário porque em
   `post-install` o StatefulSet existe mas o pod pode ainda estar subindo.
4. O Job lê `DATABASE_URL` do mesmo Secret que o Deployment usa.
5. Em sucesso (exit 0): Helm declara install completo.
6. Em falha (exit 1): Helm marca release como FAILED.
7. `golang-migrate` usa advisory lock internamente — múltiplos Jobs em retry
   não corrompem.

**Por que post-install e não pre-install?** Com PostgreSQL como template nativo
(não subchart), o StatefulSet é um recurso regular aplicado DEPOIS dos hooks
pre-install. Em pre-install, o DNS do Service do postgres não resolveria.
Post-install garante que o StatefulSet já existe.

## Secrets — onde vive o quê

| Secret | Origem | Montagem |
|--------|--------|----------|
| `DATABASE_URL` | ExternalSecret (Vault/SM) ou `existingSecret` ou inline (smoke) | env var no container |
| `KEK` (master AES-GCM) | ExternalSecret → Secret K8s → arquivo readOnly `/etc/aipp/kek` (defaultMode 0400) | arquivo, NÃO env var |
| `token_pepper` (HMAC) | ExternalSecret → Secret K8s → arquivo readOnly `/etc/aipp/token_pepper` | arquivo, NÃO env var |

**Por que KEK como arquivo e não env var**: env vars vazam em `/proc/<pid>/environ`
(acessível a quem tem acesso ao node/pod). Volume readOnly tmpfs tem exposição menor.
Defesa em profundidade que custa zero (Security Architect T1 §9.1).

## Quando escalar para ≥2 réplicas (Opção B)

O chart NÃO inclui HPA/PDB/topologySpread com 1 réplica (sem efeito útil).
Para ativar (junto com Redis mandatório), descomentar as seções em `values.yaml`:

```yaml
podDisruptionBudget:
  enabled: true
  minAvailable: 1

autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 5
  targetCPUUtilizationPercentage: 60

affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchLabels:
              app.kubernetes.io/name: ai-pool
          topologyKey: topology.kubernetes.io/zone
```

Veja `ROADMAP.md` para os gatilhos (SLA duro, RPS>50/s, quotas distribuídas).

## Rollback

```bash
helm rollback ai-pool --namespace ai-pool
```

O chart mantém `revisionHistoryLimit: 10` para permitir rollback rápido.

## Observability

### Logging (ativo desde o MVP)

- **JSON estruturado** em stdout (12-factor) — capturado pelo runtime K8s.
- **SanitizingLogger** (F7) redige automaticamente `authorization`, `api_key`,
  `bearer`, headers sensíveis. Chaves de provedores NUNCA aparecem em logs.
- Campos padrão: `timestamp`, `level`, `msg`, `request_id`, `user_id`,
  `provider`, `model`, `latency_ms`, `status`.
- Em K8s: stdout/stderr → coletado pelo DaemonSet de logging.

### Stack recomendada (Loki + Grafana)

Para ~30 usuários internos, a stack **mais leve e K8s-nativa** é:

```
App (stdout JSON) → Fluent-bit (DaemonSet) → Loki (single-instance)
                                              ↓
                                          Grafana (dashboards + alertas)
```

**Por que Loki (não ELK)**: Loki indexa APENAS labels (não conteúdo), então
consome ~10x menos recursos que Elasticsearch para o mesmo volume. Para 30
usuários é mais que suficiente.

Manifests de referência (não incluídos neste chart para manter separação):
- [Loki Helm chart](https://github.com/grafana/loki/blob/main/production/helm/loki)
- [Fluent-bit Helm chart](https://docs.fluentbit.io/manual/installation/kubernetes)
- [Grafana Helm chart](https://grafana.github.io/helm-charts)

Query de exemplo (Logs do app com erro 5xx):

```logql
{container="ai-pool"} |= "level=ERROR" or json_status >= 500
```

### Prometheus metrics (preparado, gated no MVP)

A aplicação já tem **instrumentação Prometheus completa** desde F11 (counters
+ histograms em todos os pontos críticos). O endpoint `/metrics` fica
**desativado por padrão** (env `ENABLE_METRICS=false`); a instrumentação
sempre roda no binário, mas o endpoint só é exposto quando o cluster tem
Prometheus + Grafana prontos para consumi-lo.

**Para ativar o `/metrics`:**

1. Setar env no Deployment (via `values.yaml`):
   ```yaml
   app:
     enableMetrics: "true"
   ```
2. Garantir que a annotation `prometheus.io/scrape: "true"` esteja nas pods
   (o deployment.yaml já vem com `prometheus.io/scrape: "false"` — flip para
   `true` no `values-prod.yaml` quando ativar). Ou criar um **ServiceMonitor**:

   ```yaml
   # ServiceMonitor para Prometheus Operator (kube-prometheus-stack).
   # Salvar como template adicional no chart quando Prometheus chegar.
   apiVersion: monitoring.coreos.com/v1
   kind: ServiceMonitor
   metadata:
     name: ai-pool
   spec:
     selector:
       matchLabels:
         app.kubernetes.io/name: ai-pool
     endpoints:
       - port: http
         path: /metrics
         interval: 30s
   ```

**Métricas disponíveis** (todas prefixadas com `aipp_`):

| Métrica | Tipo | Labels | Descrição |
|---------|------|--------|-----------|
| `aipp_proxy_requests_total` | counter | provider, model, status | requests que chegam ao /v1/* |
| `aipp_proxy_request_duration_seconds` | histogram | provider, model | latência total (50ms a 300s) |
| `aipp_failover_attempts_total` | counter | provider | tentativas ao upstream (1 req com failover = 2+) |
| `aipp_tokens_total` | counter | provider, model, type | tokens (prompt/completion/cached) |
| `aipp_keys_disabled_total` | counter | reason | desabilitações (auto/manual) |
| `aipp_key_reactivations_total` | counter | — | reativações pelo scheduler |

**Alertas sugeridos (quando Prometheus chegar):**

| Alerta | Expr (sketch) | Severidade |
|--------|--------------|-----------|
| Alta taxa de 5xx | `rate(aipp_proxy_requests_total{status=~"5.."}[5m]) / rate(aipp_proxy_requests_total[5m]) > 0.05` | page |
| Latência p99 alta | `histogram_quantile(0.99, rate(aipp_proxy_request_duration_seconds_bucket[5m])) > 30` | warn |
| Falha nas chaves de um provider | `rate(aipp_keys_disabled_total{reason="auto"}[10m]) > 5` | page |
| Sem reativações há muito tempo | `increase(aipp_key_reactivations_total[1h]) == 0 and aipp_keys_disabled_total > 0` | warn |
| Tokens consumidos anomalia | `rate(aipp_tokens_total[10m]) > 5 * avg_over_time(rate(aipp_tokens_total[10m])[1d:10m])` | warn |

### Distributed tracing (preparado para ROADMAP)

- **OpenTelemetry SDK**: a applicação usa `slog` com `request_id`/`trace_id`
  nos logs desde F8. Quando a stack de tracing chegar (Tempo/Jaeger), basta
  adicionar `otelhttp` middleware + exporter.
- Nada neste chart bloqueia a adoção futura.

## Troubleshooting

| Sintoma | Causa provável | Ação |
|---------|----------------|------|
| Pod CrashLoopBackOff logo após subir | KEK/pepper/DB secret não montados ou vazios | `kubectl describe pod` + `kubectl logs` |
| Migrations Job falha | `DATABASE_URL` inacessível, credenciais erradas, schema dirty | `kubectl logs job/<release>-migrate` |
| 502 em deploy rolling | `terminationGracePeriodSeconds` curto, ou app sem graceful shutdown | subir para 90s; confirmar `SHUTDOWN_TIMEOUT` env |
| SSE/streaming quebra | Ingress sem `proxy-buffering: off` | confirmar annotations no Ingress |
| 401 em `/v1/*` | Token inválido/revogado; ou `token_pepper` mudou (tokens antigos quebram) | regenerar tokens |
