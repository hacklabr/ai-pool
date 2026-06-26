# Helm Chart — AI Provider Pool (produção)

Helm chart para deploy do AI Provider Pool em Kubernetes. Especificação técnica
de referência: `/.mesa/specifications/spec-388d67e6.md` §3.5.2.

## Decisões arquiteturais (consolidadas pelos especialistas)

- **1 réplica** (Opção A — single-replica MVP). HPA/PDB omitidos (sem efeito com 1 pod).
- **RollingUpdate** `maxSurge:1 maxUnavailable:0` + graceful shutdown = zero-downtime.
- **Imagem `distroless/base-debian12:nonroot`** (UID 65532, CA roots p/ HTTPS).
- **Hardening**: `readOnlyRootFilesystem`, `runAsNonRoot`, `drop ALL caps`,
  `seccompProfile: RuntimeDefault`, `automountServiceAccountToken: false`.
- **Migrations via Helm Job hook** (`pre-install,pre-upgrade`) rodando `aipp --migrate-only`
  ANTES de qualquer pod subir. Falha no Job aborta o deploy.
- **Secrets**: External Secrets Operator (gold standard). KEK + token_pepper
  montados como **arquivos readOnly** (defesa em profundidade — não como env var).
- **Ingress** com annotations SSE críticas (`proxy-buffering: off`, timeouts 1h).
- **NetworkPolicy** DenyAll default + liberações explícitas (DB, DNS, HTTPS externo).
- **PostgreSQL gerenciado** (CloudSQL/RDS/Azure DB) — DATABASE_URL via Secret.

## Pré-requisitos no cluster

1. **Ingress controller** nginx (ou compatível que respeite annotations `nginx.ingress.kubernetes.io/*`).
2. **cert-manager** (opcional, mas recomendado para TLS via Let's Encrypt).
3. **External Secrets Operator** + **SecretStore/ClusterSecretStore** apontando
   para seu backend (Vault, AWS SM, GCP SM, Azure KV). Necessário se
   `externalSecret.enabled=true`.
4. **CNI com NetworkPolicy** (Calico, Cilium, AWS VPC CNI, etc.) — se
   `networkPolicy.enabled=true`.
5. **PostgreSQL** acessível pelo cluster (managed DB ou in-cluster StatefulSet).

## Instalação rápida

```bash
# Cenário 1: smoke test com DATABASE_URL inline (NÃO use em prod real)
helm upgrade --install ai-pool deploy/helm/ai-pool \
  --namespace ai-pool --create-namespace \
  --set image.tag=0.1.0 \
  --set database.url="postgres://user:pass@host:5432/aipp?sslmode=require"

# Cenário 2: produção com ExternalSecret + Ingress + NetworkPolicy
helm upgrade --install ai-pool deploy/helm/ai-pool \
  --namespace ai-pool --create-namespace \
  --set image.tag=0.1.0 \
  --set externalSecret.enabled=true \
  --set externalSecret.secretStoreRef.name=vault-backend \
  --set externalSecret.secretStoreRef.kind=ClusterSecretStore \
  --set ingress.enabled=true \
  --set 'ingress.hosts[0].host=api.example.com' \
  --set 'ingress.tls[0].secretName=ai-pool-tls' \
  --set 'ingress.tls[0].hosts[0]=api.example.com' \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.egress.databaseCIDR=10.0.0.0/8'

# Cenário 3: você já tem o Secret do DB criado (ESO fora do chart ou manualmente)
helm upgrade --install ai-pool deploy/helm/ai-pool \
  --namespace ai-pool --create-namespace \
  --set database.existingSecret=ai-pool-db-prod
```

Após o install, o `helm status` mostra as instruções (`NOTES.txt`) com smoke-tests.

## Estrutura

```
ai-pool/
├── Chart.yaml              # name=ai-pool, version=0.1.0, appVersion=0.1.0
├── values.yaml             # defaults (replicaCount=1, distroless nonroot, etc.)
└── templates/
    ├── _helpers.tpl        # nomes canonical, labels
    ├── configmap.yaml      # config NÃO-sensível (HTTP_ADDR, DEV_MODE, LOG_*, ...)
    ├── secret.yaml         # DATABASE_URL inline (apenas smoke test; ver valores)
    ├── externalsecret.yaml # ESO → Secret K8s (KEK + pepper + DATABASE_URL)
    ├── deployment.yaml     # RollingUpdate, probes, securityContext, volumes
    ├── service.yaml        # ClusterIP porta 80
    ├── ingress.yaml        # nginx-ingress com annotations SSE
    ├── migration-job.yaml  # Helm hook pre-install/pre-upgrade (aipp --migrate-only)
    ├── networkpolicy.yaml  # DenyAll + egress DB/DNS/HTTPS
    ├── serviceaccount.yaml # SA sem RBAC (app não fala com K8s API)
    └── NOTES.txt           # instruções pós-install (smoke tests, bootstrap admin)
```

## Validação local (sem cluster)

```bash
make helm-lint                                              # valida
make helm-template HELM_SETS="--set ingress.enabled=true"   # renderiza
```

## Migrations — como funciona

1. O binário `aipp` embarca as migrations via `//go:embed` (migrations/embed.go).
2. Helm cria um **Job** (`pre-install,pre-upgrade` hook) que roda `aipp --migrate-only`.
3. O Job lê `DATABASE_URL` do mesmo Secret que o Deployment usará.
4. Em sucesso (exit 0): Helm prossegue com o deploy.
5. Em falha (exit 1): Helm aborta — nenhum pod novo sobe com schema inconsistente.
6. `golang-migrate` usa advisory lock internamente — múltiplos Jobs em retry
   não corrompem.

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
