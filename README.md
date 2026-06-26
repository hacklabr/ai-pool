# AI Provider Pool

Um **proxy/load-balancer de provedores de LLM compatível com a API OpenAI**, que
centraliza e governa o acesso a múltiplos provedores (Gemini, Mistral, DeepSeek,
Z.ai, Kimi). Distribui carga entre chaves de API do mesmo provedor, garante
resiliência via failover transparente, aproveita cache de contexto dos LLMs via
afinidade de roteamento, e desabilita automaticamente chaves problemáticas
reativando-as quando a cota é liberada.

**Stack:** Go + Beego (admin/UI) + HTTP puro (proxy core) + PostgreSQL.
**Licença:** MIT.
**Especificação técnica:** [`docs/spec.md`](.mesa/specifications/spec-388d67e6.md).

## Motivação

Membros da equipe ficavam sem tokens em seus planos individuais enquanto outros
tinham saldo ocioso. Chaves eram compartilhadas informalmente via Telegram —
prática insegura e sem governança. O pool elimina ambos os problemas com
distribuição inteligente de carga e governança por usuário/token.

## Estrutura do projeto (Arquitetura Hexagonal / Ports & Adapters)

```
ai-pool/
├── cmd/
│   └── aipp/
│       └── main.go                # entrypoint: monta adapters, sobe servidor, graceful shutdown
├── internal/
│   ├── domain/                    # LÓGICA DE NEGÓCIO PURA (sem framework/infra)
│   │   ├── doc.go                 # documentação do pacote
│   │   ├── entities.go            # User, APIToken, Provider, ProviderKey, Model
│   │   └── stores.go              # PORTS: LRUStore, AffinityStore, RateLimiter, TokenValidator
│   ├── adapters/                  # IMPLEMENTAÇÕES CONCRETAS (injetáveis no domain)
│   │   ├── httpserver/
│   │   │   ├── proxy/             # Proxy Core (net/http PURO + http.Flusher para SSE)
│   │   │   └── admin/             # UI Admin (Beego controllers + HTMX)
│   │   └── store/                 # memoryStore (LRU, afinidade, rate-limit, token stub)
│   │       ├── lru_memory.go
│   │       ├── affinity_memory.go
│   │       ├── ratelimit_memory.go
│   │       └── token_noop.go      # stub; substituído por postgres.TokenValidator em F4
│   └── config/                    # carregamento de config (env vars + defaults)
├── migrations/                    # golang-migrate SQL (schema real em F2)
├── web/                           # templates + static assets da UI (embed.FS em F8)
├── .env.example                   # variáveis de ambiente para dev local
├── .gitignore
├── go.mod / go.sum
├── LICENSE                        # MIT
└── README.md                      # este arquivo
```

### Princípios

- **`domain/` é puro Go**: sem `beego`, sem `pgx`, sem `net/http`. Define apenas
  entidades, value objects, serviços de domínio e PORTS (interfaces). Testável
  sem infra.
- **Adapters implementam interfaces de `domain/`**: dependência sempre aponta
  para dentro. Troca de PostgreSQL→outro, ou memoryStore→RedisStore é localizada.
- **Beego é um detalhe de transporte**: usado apenas para admin/UI. O Proxy Core
  é net/http puro (com `http.Flusher`) para evitar buffering no streaming SSE.
- **Store abstrações desde o MVP**: `LRUStore`, `AffinityStore`, `RateLimiter`,
  `TokenValidator` são interfaces; `memoryStore` é a implementação concreta.
  **NÃO importar `go-redis`** (evita dep fantasma). Troca futura para Redis é
  cirúrgica (spec §3.1.3).

### Decisões técnicas consolidadas

| Decisão | Justificativa |
|---------|---------------|
| 1 réplica em produção (Opção A) | Elimina bugs de estado distribuído; adequado para ~30 usuários sem SLA. Graceful shutdown + RollingUpdate dá zero-downtime planejado. |
| `organization_id NOT NULL DEFAULT 1` | Pavimenta multi-tenancy futuro sem migração cara em `usage_logs` particionada. |
| Tokens `sk-aipp-...` com HMAC-SHA256 + pepper | Defesa em profundidade contra dump do DB. |
| DB hit por request (sem cache de token) | Revogação instantânea e determinística; latência sub-ms é ruído. |
| Envelope encryption (AES-256-GCM + KEK externo) | Vazamento do dump do DB não expõe chaves de provedores. |
| `pgx` v5 + `pgxpool` + golang-migrate | Driver nativo PG mais performático em Go; migrations versionadas. |
| Streaming SSE com 3 timeouts | first-byte, idle entre chunks, overall cap. Cancelamento propagado ao upstream poupa tokens. |
| Afinidade via `X-Conversation-ID` + hash fallback | Otimização de custo real (cache de contexto do LLM). Best-effort, em memória, TTL 30min. |

## Estado atual (Fase F4)

- ✅ Estrutura hexagonal montada (F1).
- ✅ Go module inicializado (`github.com/rafael/ai-pool`).
- ✅ Domain entities + store interfaces definidas (F1).
- ✅ memoryStore implementada (LRU, afinidade, rate-limit) (F1).
- ✅ PostgreSQL schema + migrations + pool pgx (F2).
- ✅ Repositories: User, Token, Provider, Key, Model, Usage, Audit (F2).
- ✅ **Proxy Core real**: repasse transparente OpenAI-compatible com streaming SSE
  + 3 timeouts + cancelamento propagado (F3).
- ✅ Token validation: HMAC-SHA256+pepper, DB hit por request, sem cache (F3).
- ✅ SSRF guard básico (scheme allowlist + IP blocklist) (F3).
- ✅ **Auth & RBAC completo**: Argon2id login, sessões httpOnly, CSRF double-submit,
  geração/revogação de tokens sk-aipp-..., gestão admin de users/providers/keys (F4).
- ✅ Bootstrap admin CLI (`--bootstrap-admin-username`) (F4).
- ✅ **Docker Compose dev + Dockerfile multi-stage distroless** (F9).

### Próximas fases (ver especificação §5.1)

- **F5**: Routing Engine (LRU + afinidade de cache de contexto + failover chain + auto-disable + parser registry).
- **F6**: Rate limiting + registro de uso assíncrono + rollups.
- **F7**: Envelope encryption (AES-256-GCM + KEK via ESO) + SanitizingLogger.
- **F8**: UI admin completa (8 telas) com HTMX + ECharts.
- **F10**: Helm Chart (K8s) + ESO + probes + Ingress SSE.
- **F11**: CI/CD GitHub Actions + observabilidade.

---

## Quick Start (Docker)

O caminho mais rápido para ter o sistema rodando localmente. Em ~30 segundos
você terá PostgreSQL + app + migrations + admin bootstrap prontos para teste.

### Pré-requisitos

- Docker 24+ (com BuildKit; `docker buildx version` deve responder).
- Docker Compose v2+ (`docker compose version` deve responder).
- ~500 MB de disco livre (imagens golang + postgres + distroless).

### 1. Configure o ambiente

```bash
cd ai-pool/
cp .env.docker.example .env.docker
```

Edite `.env.docker` se quiser mudar a senha do admin ou do Postgres. Os
defaults já funcionam para o primeiro teste.

### 2. (Opcional) KEK para envelope encryption

Por padrão, o ambiente dev usa `IdentityDecryptor` (chaves de provedores em
plaintext no DB — aceitável SÓ em dev). Para testar AES-256-GCM envelope
encryption desde já:

```bash
./scripts/gen-kek.sh              # gera kek-dev.key (gitignored)
```

Depois edite `.env.docker` (`KEK_PATH=/run/secrets/kek`) e descomente o
volume `./kek-dev.key:/run/secrets/kek:ro` em `docker-compose.yml`.

### 3. Suba o ambiente

```bash
docker compose --env-file .env.docker up --build
```

Na primeira execução, o build da imagem Go leva ~3-5 min (download de deps).
Builds subsequentes usam cache do BuildKit (~20-30s).

### 4. Verifique que subiu

Em outro terminal:

```bash
# App respondendo?
curl http://localhost:8088/healthz
# Esperado: {"status":"ok",...}

curl http://localhost:8088/readyz
# Esperado: {"status":"ready","checks":{"db":"ok"}}
```

### 5. Acesse o admin

Abra `http://localhost:8088/web/login` no navegador. Login com:

- **Usuário:** `admin` (ou o `AIPP_BOOTSTRAP_ADMIN_USERNAME` que você definiu)
- **Senha:** `ChangeMeNow!StrongDevPassword123` (default)

### 6. Teste o proxy end-to-end

No admin UI:

1. Crie um **Provider** (ex.: DeepSeek, base URL `https://api.deepseek.com`).
2. Adicione uma **Provider Key** (sua chave real).
3. Cadastre o **Model** (ex.: `deepseek-chat`).
4. Gere um **API Token** para o seu usuário (`sk-aipp-...`).

Depois, num terminal:

```bash
curl http://localhost:8088/v1/chat/completions \
  -H "Authorization: Bearer sk-aipp-XXXXXXXXXXXXXXXX" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-chat",
    "messages": [{"role":"user","content":"Olá!"}],
    "stream": false
  }'
```

Para streaming (SSE):

```bash
curl -N http://localhost:8088/v1/chat/completions \
  -H "Authorization: Bearer sk-aipp-XXXXXXXXXXXXXXXX" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-chat",
    "messages": [{"role":"user","content":"Conte uma piada."}],
    "stream": true
  }'
```

### Comandos úteis

```bash
# Logs da app em tempo real:
docker compose logs -f app

# Logs do Postgres:
docker compose logs -f postgres

# Reset completo (apaga o volume do Postgres):
docker compose down -v

# Subir só o Postgres (para dev local com `go run`):
docker compose up -d postgres

# Build da imagem isolado (sem subir):
docker build -t ai-provider-pool:dev .

# Entrar no Postgres (psql):
docker compose exec postgres psql -U aipp -d aipp
```

### Decisões do ambiente dev (F9)

- **Imagem runtime**: `gcr.io/distroless/base-debian12:nonroot` — tem CA roots
  (HTTPS com provedores), UID 65532 pré-configurado, sem shell (segurança).
- **Healthcheck da app**: **não há** no compose, porque distroless não tem
  shell/wget/curl. Verificação é manual via `/healthz`. Em produção, o Helm
  chart define probes (liveness/readiness/startup).
- **Migrations**: rodam automaticamente no startup do app em DEV_MODE
  (`internal/adapters/postgres.RunMigrations` via `migrations.FS` embed).
  Em produção, rodam via Helm Job hook (pre-install/pre-upgrade).
- **Admin bootstrap**: idempotente, automático no startup em DEV_MODE se
  `AIPP_BOOTSTRAP_ADMIN_USERNAME`/`PASSWORD` estão setados.

---

## Desenvolvimento local

### Setup inicial (com DB)

```bash
# 1. Copiar env de exemplo
cp .env.example .env
# Editar .env com DATABASE_URL, AIPP_TOKEN_PEPPER (32+ bytes)

# 2. Subir PostgreSQL via Docker Compose (em F9); por ora use qualquer instância.

# 3. Aplicar migrations
make migrate-up  # (em F9); por ora: ./aipp com migrate.go integrado

# 4. Criar admin inicial (idempotente)
AIPP_BOOTSTRAP_ADMIN_PASSWORD="senha-forte-12-chars" \
  ./aipp --bootstrap-admin-username=admin

# 5. Build e rodar
go build ./...
DEV_MODE=false ./aipp
```

### Modo dev sem DB (validação de estrutura)

```bash
DEV_MODE=true go run ./cmd/aipp
# /healthz → 200, /v1/* → 401 (auth stub deny-by-default), /web/* → 503 (admin precisa DB)
```

### Testes

```bash
go test -race -count=1 ./...
```

## Licença

MIT — veja [LICENSE](LICENSE).
