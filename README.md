# AI Provider Pool

Um **proxy/load-balancer de LLMs compatível com a API OpenAI**. Você cadastra
várias chaves de diversos provedores (Gemini, Mistral, DeepSeek, Z.ai, Kimi…) e o
pool distribui a carga entre elas, faz failover automático quando uma falha,
aproveita o cache de contexto dos modelos e governa quem pode usar o quê.

**Stack:** Go + PostgreSQL · **Licença:** MIT

---

## O que é

Em vez de cada um usar sua própria chave de API (e ficar sem tokens enquanto
outros têm saldo sobrando, ou compartilhar chaves pelo Telegram), você aponta
todos os clientes OpenAI-compatible para **um único endpoint**. O pool:

- **Balanceia a carga** entre as chaves habilitadas de cada provedor (LRU).
- **Falha de forma transparente** — se uma chave retorna erro, tenta a próxima.
- **Aproveita o cache de contexto** roteando uma mesma conversa para a mesma
  chave (economia real de tokens). Cada resposta vem com o header
  `X-Context-Cache: HIT|MISS` pra você ver.
- **Desliga e reativa chaves sozinho** — lê a data de liberação de cota na
  mensagem de erro do provedor e reabilita na hora certa.
- **Governança**: usuários, tokens `sk-aipp-...`, RBAC (admin/usuário), rate
  limiting, auditoria e dashboard de uso.
- **Seguro**: chaves de provedor cifradas (AES-256-GCM), tokens em hash,
  sanitização de logs, proteção SSRF.

---

## Como funciona

```
Cliente (SDK OpenAI / curl)            Provedores de LLM
        │                                       │
        │  Authorization: Bearer sk-aipp-...    │
        ▼                                       │
┌───────────────────┐    roteia p/ a chave      │
│   AI Provider Pool│ ─────────────────────────►│  Gemini / Mistral /
│  (compatível /v1) │    menos usada (LRU)       │  DeepSeek / Z.ai / Kimi
│                   │ ◄─────────────────────────│
│  · valida token   │   resposta (streaming SSE) │
│  · failover       │                            │
│  · afinidade cache│                            │
│  · registra uso   │                            │
└───────────────────┘
        │
        ▼
   PostgreSQL (chaves cifradas, uso, usuários)
```

1. O cliente chama `/v1/chat/completions` (ou `/api/...`) com um token `sk-aipp-...`.
2. O pool valida o token, resolve o modelo → provedor → chave (pela menos usada,
   respeitando afinidade de sessão via header `X-Session-Id`).
3. Repassa a requisição ao provedor (streaming SSE incluso), trocando apenas a
   chave e o User-Agent quando configurado.
4. Em caso de erro de chave, tenta a próxima; se a chave erra muito, desabilita
   e reativa na data informada pelo provedor.
5. Registra o uso (tokens, latência, cache) para o dashboard e auditoria.

Interface web admin (server-rendered, HTMX + ECharts) para gerenciar tudo:
provedores, modelos, chaves, usuários, dashboard e auditoria.

---

## Como instalar

### Local (Docker — teste rápido)

```bash
cp .env.docker.example .env.docker
docker compose --env-file .env.docker up --build
```

- UI admin: http://localhost:8088/web/login · login `admin` / `ChangeMeNow!StrongDevPassword123`
- Health: `curl http://localhost:8088/healthz`

Depois, na UI: cadastre um **provedor** + **chave** + **modelo**, gere um
**token** `sk-aipp-...` e teste:

```bash
curl http://localhost:8088/v1/chat/completions \
  -H "Authorization: Bearer sk-aipp-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"Olá!"}]}'
```

### Kubernetes (Helm — produção)

O Helm chart **inclui o PostgreSQL por padrão** — um único install sobe app + banco.
Passo a passo completo em **[INSTALL.md](INSTALL.md)**. Resumo:

```bash
# imagem (ajuste o registry)
docker build -t ghcr.io/SEU_USUARIO/ai-pool:0.1.0 . && docker push ghcr.io/SEU_USUARIO/ai-pool:0.1.0

# KEK + pepper (cifragem das chaves)
KEK=$(openssl rand -base64 32); PEPPER=$(openssl rand -base64 48)
kubectl create namespace ai-pool
kubectl -n ai-pool create secret generic ai-pool-app-secrets \
  --from-literal=kek="$KEK" --from-literal=token-pepper="$PEPPER"

# install (app + PostgreSQL embutido)
helm install ai-pool deploy/helm/ai-pool -n ai-pool \
  --set postgresql.auth.password="$(openssl rand -base64 24)" \
  --set externalSecret.appSecret.existingSecret=ai-pool-app-secrets \
  --set ingress.enabled=true --set 'ingress.hosts[0].host=ai-pool.seudominio.com'
```

Para usar um **banco gerenciado** (CloudSQL/RDS) em vez do embutido:
`--set postgresql.enabled=false --set database.existingSecret=...` (ver INSTALL.md).

---

## Desenvolvimento

```bash
go test -race -count=1 ./...          # testes
docker compose --env-file .env.docker up --build   # dev com hot rebuild
```

Variáveis de configuração: `.env.example` e `deploy/helm/ai-pool/values.yaml`.

## Licença

MIT — veja [LICENSE](LICENSE).
