# Instalação — AI Provider Pool (Kubernetes / Helm)

> O Helm chart **inclui o PostgreSQL por padrão** (subchart Bitnami). Um único
> `helm install` sobe **aplicação + banco** no seu cluster. Ideal para
> self-hosted, dev e homologação.

## Pré-requisitos
- Cluster Kubernetes + `kubectl` + `helm` (v3).
- Um **registry** acessível pelo cluster com a imagem do app (build + push abaixo).
- (Opcional) Ingress controller (nginx) e um domínio, se quiser expor via Ingress.

---

## 1. Build + push da imagem
O cluster precisa puxar a imagem de um registry. Ajuste o `repository` ao seu:
```bash
cd /home/rafael/devel/ai-pool
docker build -t ghcr.io/SEU_USUARIO/ai-pool:0.1.0 .
docker push ghcr.io/SEU_USUARIO/ai-pool:0.1.0
# (faça login no registry antes: docker login ghcr.io ...)
```
O chart referencia `ghcr.io/rafael/ai-pool:0.1.0`. Se usar outro, sobrescreva:
`--set image.repository=SEU_REGISTRY/ai-pool`.

## 2. Segredos do app: KEK + token pepper
O proxy cifra as chaves de provedores (AES-256-GCM). A chave-mestra (KEK) e o
pepper dos tokens devem existir como **arquivos** (defesa em profundidade):
```bash
KEK=$(openssl rand -base64 32)
PEPPER=$(openssl rand -base64 48)
kubectl create namespace ai-pool
kubectl -n ai-pool create secret generic ai-pool-app-secrets \
  --from-literal=kek="$KEK" --from-literal=token-pepper="$PEPPER"
```
> Em produção, materialize este Secret via **External Secrets Operator**
> (`externalSecret.enabled=true`) a partir do seu vault — ver `values.yaml`.

## 3. Instalar (app + PostgreSQL embutido)
```bash
helm install ai-pool deploy/helm/ai-pool -n ai-pool \
  --set postgresql.auth.password="$(openssl rand -base64 24)" \
  --set externalSecret.appSecret.existingSecret=ai-pool-app-secrets \
  --set ingress.enabled=true \
  --set 'ingress.hosts[0].host=ai-pool.seudominio.com'
```
O `helm install` imprime o `NOTES.txt` com os próximos passos.

O que sobe:
- **Deployment** (1 réplica, graceful shutdown, RollingUpdate zero-downtime).
- **PostgreSQL** (subchart Bitnami, PVC 10Gi, senha = a que você passou).
- **Migration Job** (`aipp --migrate-only` — roda antes da app subir).
- Service, Ingress (annotations SSE), NetworkPolicy, ServiceAccount, probes.

## 4. Pós-instalação
```bash
# migrations OK?
kubectl -n ai-pool logs job/ai-pool-migration --tail=-1 | tail -5
# pod pronto?
kubectl -n ai-pool get pods -l app.kubernetes.io/instance=ai-pool
# smoke test
kubectl -n ai-pool port-forward svc/ai-pool 8080:80 &
curl -s http://localhost:8080/healthz   # {"status":"ok"}

# bootstrap do admin (one-shot; senha ≥ 8 chars)
kubectl -n ai-pool exec deploy/ai-pool -- \
  env AIPP_BOOTSTRAP_ADMIN_PASSWORD='SuaSenhaForte123' \
  /aipp --bootstrap-admin-username=admin
```
Acesse a UI: `https://ai-pool.seudominio.com/web/login` (ou via port-forward).
Cadastre **provedores → modelos → chaves**, gere um token `sk-aipp-...` e aponte
seus clientes (OpenAI-compatible) para `https://ai-pool.seudominio.com`.

---

## Alternativa: DB gerenciado (CloudSQL/RDS) — produção
Se preferir um Postgres gerenciado em vez do embutido:
```bash
kubectl -n ai-pool create secret generic ai-pool-db \
  --from-literal=DATABASE_URL='postgres://USER:PASS@cloudsql:5432/aipp?sslmode=require'

helm install ai-pool deploy/helm/ai-pool -n ai-pool \
  --set postgresql.enabled=false \
  --set database.existingSecret=ai-pool-db \
  --set externalSecret.appSecret.existingSecret=ai-pool-app-secrets \
  --set ingress.enabled=true --set 'ingress.hosts[0].host=ai-pool.seudominio.com'
```

## Operações úteis
```bash
helm upgrade ai-pool deploy/helm/ai-pool -n ai-pool -f values-prod.yaml   # atualizar
helm rollback ai-pool -n ai-pool                                            # rollback
kubectl -n ai-pool logs -f deploy/ai-pool                                   # logs
helm uninstall ai-pool -n ai-pool                                           # remover (NÃO apaga o PVC por default)
```

> O PVC do PostgreSQL **não é removido** no `helm uninstall` (preserva dados).
> Para resetar: `kubectl -n ai-pool delete pvc -l app.kubernetes.io/name=postgresql`.
