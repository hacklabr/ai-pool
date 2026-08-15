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
- **Onboarding por convite**: o admin gera um link de uso único e o novo
  usuário se cadastra — e já configura o cliente — com um único
  `curl -fsSL <url>/invite/<token> | bash`.
- **Catálogo auto-atualizado**: job periódico sincroniza a lista de modelos
  com cada provedor (`GET /models`), e o cliente OpenCode atualiza sua lista
  com `curl -fsSL <url>/setup/update-models.sh | bash`.
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

## Onboarding por convite (`curl | bash`)

Novo usuário entra **sem tocar na UI admin**:

1. O admin, na tela Usuários, cria um convite informando só o **username** e
   recebe um comando de setup único:

   ```bash
   curl -fsSL https://seu-pool.exemplo/invite/inv-XXXXXXXX... | bash
   ```

2. O novo usuário roda o comando na própria máquina. O script:
   - pede uma senha (mín. 12 caracteres), lida direto do terminal;
   - pergunta se quer configurar **OpenCode** e/ou **MiMo-Code**
     automaticamente;
   - aceita o convite via API — o servidor cria o usuário (Argon2id) e um
     token `sk-aipp-...` em uma transação — e grava o provider `llm-pool`
     (com baseURL, token e catálogo de modelos) em
     `~/.config/opencode/opencode.json` e/ou `~/.config/mimocode/mimocode.json`.

Detalhes de segurança: token de convite `inv-...` de **uso único**, expira em
**24 h**, guardado só como SHA-256; `GET /invite/{token}` devolve sempre o
mesmo script (anti-enumeração); aceite rate-limitado por IP; convites
pendentes são revogáveis na UI admin.

## Catálogo de modelos sempre atualizado

O catálogo de modelos (tabela `models`) é mantido em sincronia com os
provedores de três formas complementares:

- **Job periódico in-process** consulta `GET {base_url}/models` de cada
  provedor ativo e reconcilia o catálogo (a cada 6 h por padrão, via
  `AIPP_MODEL_SYNC_INTERVAL`). Fail-safe: provedor indisponível ⇒ nenhuma
  alteração. Nunca deleta modelos nem sobrescreve enriquecimento manual —
  apenas adiciona novos, reativa os que voltaram e desativa os ausentes.
- **Dicionário interno de especificações** (`specs.json` embutido no binário):
  modelos novos já nascem com `context_window`, `output_limit` e modalidades
  preenchidos (dados das docs oficiais dos provedores); modelos existentes só
  recebem backfill de campos vazios.
- **Botão "Sincronizar agora"** na tela de modelos do admin, e CLI idempotente
  `aipp --sync-models` (usado pelo Helm no pós-instalação — instalação nasce
  com o catálogo populado).

Clientes consultam o catálogo agregado em **`GET /v1/models`** (formato
OpenAI; `?format=opencode` devolve o schema do OpenCode). E para atualizar a
lista de modelos no cliente OpenCode sem editar arquivo nem digitar token:

```bash
curl -fsSL https://seu-pool.exemplo/setup/update-models.sh | bash
```

O script lê o token do próprio `opencode.json` do usuário, faz backup do
arquivo e reescreve apenas `provider["llm-pool"].models` via `jq` (requer
`jq` instalado).

---

## Planos e cotas por chave

Chaves podem ter um **plano** associado (nome, data de renovação e cota de
tokens por ciclo — ex.: GLM Coding Lite, Kimi Code). Quando uma chave é
desativada por teto de cota, o pool persiste a soma de tokens da janela como
medição observada, e o dashboard admin mostra **uso do plano por ciclo,
calibração empírica do denominador e subutilização** — na prática, quanto de
cota contratada está sobrando.

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
  --from-literal=kek="$KEK" --from-literal=token_pepper="$PEPPER"

# (registry PRIVADO) image pull secret — pule se a imagem for pública
kubectl -n ai-pool create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username=SEU_GITHUB_USER \
  --docker-password=SEU_PAT_COM_READ_PACKAGES \
  --docker-email=voce@email.com

# install (app + PostgreSQL embutido)
helm install ai-pool deploy/helm/ai-pool -n ai-pool \
  --set postgresql.auth.password="$(openssl rand -base64 24)" \
  --set externalSecret.appSecret.existingSecret=ai-pool-app-secrets \
  --set 'image.pullSecrets[0].name=ghcr-pull' \
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

Documentação de referência: [docs/reference/prd.md](docs/reference/prd.md)
(requisitos funcionais), [docs/reference/architecture.md](docs/reference/architecture.md)
e [docs/reference/jornadas.md](docs/reference/jornadas.md) (fluxos de usuário).

## Licença

MIT — veja [LICENSE](LICENSE).
