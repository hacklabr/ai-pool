# Instalação — AI Provider Pool (Kubernetes / Helm)

> Desde a **v0.2.0** do chart, o PostgreSQL é entregue como **templates
> nativos** (imagem oficial `postgres:16-alpine`), seguindo o padrão Hacklab.
> O subchart Bitnami foi removido. Um único `helm install` sobe **aplicação +
> banco** no seu cluster.

## Pré-requisitos
- Cluster Kubernetes + `kubectl` + `helm` (v3+).
- **Ingress controller** nginx (ou compatível).
- **cert-manager** + **ClusterIssuer** `letsencrypt-prod`.
- **StorageClass** para o PVC do PostgreSQL (ex.: `ceph-blockstorage`).
- (Opcional) **Velero** com fs-backup para backup do volume do postgres.

---

## 1. KEK + token pepper (PRODUÇÃO — obrigatório)

O proxy cifra as chaves de provedores (AES-256-GCM). A chave-mestra (KEK) e o
pepper dos tokens devem ser **gerados uma única vez** e **salvos em cofre
offline** (perdeu = perdeu todas as chaves cifradas no DB).

```bash
# Gere os segredos (salve a saída em cofre offline AGORA):
KEK=$(openssl rand -base64 32)
PEPPER=$(openssl rand -base64 48)
echo "KEK=$KEK"
echo "PEPPER=$PEPPER"

# Crie o namespace + Secret K8s:
kubectl create namespace hacklab-ai-pool
kubectl -n hacklab-ai-pool create secret generic hacklab-ai-pool-secrets \
  --from-literal=kek="$KEK" \
  --from-literal=token_pepper="$PEPPER"
```

> O nome do Secret **deriva do release name** (`<release>-secrets`).
> Com `helm install hacklab-ai-pool ...` → Secret `hacklab-ai-pool-secrets`.
> Com `helm install ai-pool ...` → Secret `ai-pool-secrets`.
>
> Em produção real, materialize este Secret via **External Secrets Operator**
> (`externalSecret.enabled=true`) a partir do seu vault.

## 2. Image pull secret (REGISTRY PRIVADO — obrigatório se a imagem não for pública)

Se a imagem estiver em um registry **privado** (ex.: `ghcr.io/hacklabr/ai-pool`
com package privado, ECR, GCR, Harbor), o Kubernetes precisa de credenciais
para fazer o `pull`. Crie um Secret `docker-registry` no namespace antes do
`helm install`:

```bash
# GitHub Container Registry (GHCR):
kubectl -n hacklab-ai-pool create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username=SEU_GITHUB_USER \
  --docker-password=SEU_PAT_COM_READ_PACKAGES \
  --docker-email=voce@email.com

# Ou, para qualquer registry, usando ~/.docker/config.json:
kubectl -n hacklab-ai-pool create secret generic ghcr-pull \
  --from-file=.dockerconfigjson=$HOME/.docker/config.json \
  --type=kubernetes.io/dockerconfigjson
```

> **PAT do GitHub** precisa ter escopo `read:packages`. Crie em:
> https://github.com/settings/tokens (classic) — válida por 1 ano, **rotacione**.

Depois referencie no `helm install` (ou em `values-prod.yaml`):

```bash
helm install hacklab-ai-pool deploy/helm/ai-pool \
  -n hacklab-ai-pool \
  -f deploy/helm/ai-pool/values-prod.yaml \
  --set 'image.pullSecrets[0].name=ghcr-pull'
```

Ou, equivalente em `values-prod.yaml`:
```yaml
image:
  pullSecrets:
    - name: ghcr-pull
```

> **Registry público**: pule esta etapa. O chart vem com `image.pullSecrets: []`
> por default e o Kubernetes baixa a imagem sem auth.

## 3. Instalar (app + PostgreSQL embutido)

```bash
helm install hacklab-ai-pool deploy/helm/ai-pool \
  -n hacklab-ai-pool \
  -f deploy/helm/ai-pool/values-prod.yaml
```

O `helm install` imprime o `NOTES.txt` com os próximos passos.

O que sobe:
- **Deployment** (1 réplica, graceful shutdown, RollingUpdate zero-downtime).
- **PostgreSQL StatefulSet** (imagem oficial `postgres:16-alpine`, PVC
  `ceph-blockstorage`, senha auto-gerada e persistida via `lookup`).
- **Migration Job** (`post-install` hook — init container `wait-for-db`
  espera o postgres ficar Ready, depois roda `aipp --migrate-only`).
- Service, Ingress (nginx + cert-manager + annotations SSE),
  NetworkPolicy (opcional), ServiceAccount, probes.

## 4. Pós-instalação

```bash
# migrations OK? (post-install hook — pode levar 1-2 min na 1ª instalação)
kubectl -n hacklab-ai-pool logs job/hacklab-ai-pool-migrate --tail=-1 | tail -10
# Look for: "migrations applied successfully"

# pods prontos?
kubectl -n hacklab-ai-pool get pods
# Expect: ai-pool-postgres-0 Running, hacklab-ai-pool-xxxxx Running, Job Completed

# smoke test
kubectl -n hacklab-ai-pool port-forward svc/hacklab-ai-pool 8080:80 &
curl -s http://localhost:8080/healthz   # {"status":"ok"}

# bootstrap do admin (DEV_MODE=false: senha ≥12 chars via env)
kubectl -n hacklab-ai-pool exec deploy/hacklab-ai-pool -- \
  env AIPP_BOOTSTRAP_ADMIN_PASSWORD='SuaSenhaForte123' \
  /aipp --bootstrap-admin-username=admin
```

Acesse a UI: `https://ia.hacklab.com.br/web/login`
Cadastre **provedores → modelos → chaves**, gere um token `sk-aipp-...` e aponte
seus clientes (OpenAI-compatible) para `https://ia.hacklab.com.br`.

---

## Alternativa: DB gerenciado (CloudSQL/RDS) — produção

Se preferir um Postgres gerenciado em vez do embutido:

```bash
kubectl -n hacklab-ai-pool create secret generic ai-pool-db \
  --from-literal=DATABASE_URL='postgres://USER:PASS@cloudsql:5432/aipp?sslmode=require'

helm install hacklab-ai-pool deploy/helm/ai-pool \
  -n hacklab-ai-pool \
  -f deploy/helm/ai-pool/values-prod.yaml \
  --set postgresql.enabled=false \
  --set database.existingSecret=ai-pool-db
```

## Operações úteis

```bash
helm upgrade hacklab-ai-pool deploy/helm/ai-pool -n hacklab-ai-pool -f deploy/helm/ai-pool/values-prod.yaml
helm rollback hacklab-ai-pool -n hacklab-ai-pool
kubectl -n hacklab-ai-pool logs -f deploy/hacklab-ai-pool
helm uninstall hacklab-ai-pool -n hacklab-ai-pool
```

> **Atenção:** o StatefulSet e PVC do PostgreSQL têm `helm.sh/resource-policy: keep`
> e **NÃO são removidos** no `helm uninstall` (preserva dados). Para resetar:
> ```bash
> kubectl -n hacklab-ai-pool delete statefulset hacklab-ai-pool-postgresql
> kubectl -n hacklab-ai-pool delete pvc -l app.kubernetes.io/component=postgresql
> ```

## Senha do PostgreSQL — rotação

A senha é auto-gerada na 1ª instalação e persistida via `lookup` entre upgrades.
Para rotacionar:

```bash
NEW_PASS=$(openssl rand -base64 24)
helm upgrade hacklab-ai-pool deploy/helm/ai-pool -n hacklab-ai-pool \
  -f deploy/helm/ai-pool/values-prod.yaml \
  --set postgresql.auth.password="$NEW_PASS"
# O Secret "<release>-db" é regenerado com nova senha + DATABASE_URL em sync.
# O postgres pod precisa ser restartado para aplicar: kubectl delete pod hacklab-ai-pool-postgresql-0
```

## Validação local (sem cluster)

```bash
make helm-lint                                              # valida
make helm-template HELM_SETS="--set ingress.enabled=true"   # renderiza
helm template hacklab-ai-pool deploy/helm/ai-pool \
  -f deploy/helm/ai-pool/values-prod.yaml                   # renderiza com prod values
```
