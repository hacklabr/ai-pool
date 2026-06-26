# Analysis: Security Architect — Turn 2

**Agent ID:** security-security-architect
**Timestamp:** 2026-06-25T18:55:53.887Z

# Delta — Security Architect (Turno 2)

> Foco: convergir tensões técnicas e confirmar alinhamentos. Estou revisando 5 análises de pares. Vou direto ao ponto, sem repetir o que já está consolidado.

---

## T1 — Hashing de tokens `sk-...`: HMAC+pepper vs SHA-256 vs Argon2id

### Posições divergentes
- **DBA** (§2.3): `token_hash = SHA-256(token)` com UNIQUE → lookup O(1). Justifica: token de alta entropia → brute-force inviável → SHA-256 determinístico basta.
- **Eu** (Turno 1, §4.2): `key_id` público + `HMAC-SHA256(secret, pepper)` → lookup por key_id, verificação constant-time.
- **Backend** (§10.4): "hash (Argon2id ou bcrypt), como senhas".
- **Software Architect** (§5): "token_hash UNIQUE — nunca em claro, hashear" (vago).

### Veredito: HMAC-SHA256 com pepper é a recomendação final — mas SHA-256 puro é aceitável.

**Por que NÃO Argon2id/bcrypt para tokens (corrige o Backend):**

O Backend comete um erro técnico aqui. Argon2id e bcrypt são funções de **password hashing** projetadas para serem **lentas** (10–100ms por hash) e usam **salt aleatório** por hash. Aplicadas a tokens de API:

1. **Salt aleatório quebra lookup direto**: com salt único por linha, não dá para fazer `WHERE token_hash = ?`. Teria que iterar todas as linhas e verificar uma a uma. Para 30 usuários é viável, mas escala péssimo e quebra o requisito O(1).
2. **Latência**: 10–100ms por request de proxy só para validar token → inaceitável no hot path (latência percebida é o atributo crítico, conforme o próprio Backend destacou em P3).
3. **Não há benefício de segurança**: Argon2id existe para tornar brute-force de senhas **de baixa entropia** (humanas, ~20-40 bits) caro. Tokens CSPRNG de 285 bits têm entropia suficiente para tornar qualquer hash (mesmo SHA-256 rápido) inquebrável por brute-force. Aplicar KDF cara aqui é desperdício puro.

**Comparação HMAC+pepper vs SHA-256 puro:**

| Critério | SHA-256 puro (DBA) | HMAC-SHA256+pepper (meu Turno 1) |
|---|---|---|
| Lookup O(1) via UNIQUE | ✅ Sim | ✅ Sim (HMAC é determinístico com pepper fixo) |
| Latência | ~µs | ~µs (mesma ordem) |
| Custo de implementação | Menor | Ligeiramente maior (gerir pepper) |
| Protege contra rainbow tables | Parcial (alta entropia já protege) | Sim |
| Defesa se DB vazar mas pepper não | **Não** — quem tem dump valida qualquer token que capture depois | **Sim** — pepper separa "DB comprometido" de "segredo comprometido" |
| Defesa contra "confirmation attack" | Fraca — atacante com DB dump pode confirmar tokens roubados de logs/screenshots | Forte — sem pepper, hashes no DB são inúteis |

**Análise de risco do "confirmation attack"** (cenário realista):
1. Atacante obtém dump do DB (SQLi, backup vazado, insider).
2. Atacante também tem um token `sk-...` capturado em log de aplicação cliente, screenshot, ou `.env` commitado (cenário comum — este é o vetor nº 1 de vazamento de tokens de API).
3. **Com SHA-256 puro**: atacante computa `SHA-256(token_capturado)` e verifica se está no dump → **confirma que aquele token é válido no sistema** e qual usuário ele pertence. Pode agora usar o token até ser revogado.
4. **Com HMAC+pepper**: atacante tem hash mas não pepper → não consegue nem confirmar que o token capturado é válido → token capturado não pode ser correlacionado com o dump.

Este cenário é suficientemente realista para justificar o custo marginal do pepper. O pepper vive no mesmo secret store do KEK (K8s Secret via ESO/Sealed Secrets), junto da chave de criptografia das chaves de provedor.

### Recomendação FINAL para o spec
- **Formato do token**: `sk-aipp-[keyid8]-[secret48]` (Turno 1, §4.1). Mantém lookup O(1) por `key_id` (não-sensível) e verificação por HMAC do `secret`.
- **Storage**: `token_hash = HMAC-SHA256(secret, PEPPER)` como BYTEA com UNIQUE em `(user_id, key_id)` ativo. Alternativa aceitável se houver objeção ao split key_id/secret: `token_hash = HMAC-SHA256(full_token, PEPPER)` com UNIQUE direto.
- **Pepper**: variável de ambiente `TOKEN_HASH_PEPPER`, 32 bytes, vivendo no mesmo secret store do KEK.
- **Comparação**: `hmac.Equal` (constant-time), nunca `==`.
- **PROIBIDO**: Argon2id/bcrypt para tokens (apenas para senhas de login).
- **Concordo com o DBA** em tudo exceto na ausência do pepper. A justificativa do DBA ("alta entropia torna SHA-256 suficiente") está correta do ponto de vista de brute-force, mas ignora o confirmation attack.

> **Ação direta para o Backend**: corrigir a proposta de Argon2id/bcrypt para tokens na sua seção §10.4. Aplicar Argon2id apenas em `users.password_hash` (que está correto).

---

## T2 — Cache de token: DB hit por request vs Redis

### Alinhamento quase total
- **Backend** (P3, §4.5): volume baixo, DB hit por request trivial.
- **DBA** (§4.1): UNIQUE em `token_hash` → lookup B-tree O(log N) cacheado, sub-milissegundo.
- **Eu** (Turno 1, §4.4): MVP sem cache positivo, DB hit por request.
- **DevOps** (§9): propõe Store abstraction (memory + Redis) já no MVP para multi-réplica.

### Veredito: MVP = DB hit por request, mas com Store abstraction desde já

A aparente contradição entre "sem cache" (Backend/eu) e "Store abstraction" (DevOps) é falsa:

- **MVP single-réplica**: DB hit por request é a escolha correta. Justificativas:
  1. Garante **revogação instantânea** (requisito explícito do briefing) sem complexidade de invalidação.
  2. Latência sub-milissegundo com índice UNIQUE → irrelevante no hot path (a chamada ao provedor domina, 100ms–10s).
  3. Simplicidade vence para 30 usuários.

- **Store abstraction desde já**: **concordo com o DevOps**. Não custa definir uma interface `TokenValidator` com duas implementações (`DBValidator`, `RedisValidator` futura). Quando escalar para multi-réplica, basta injetar a implementação Redis com cache positivo curto (30s) + pub/sub de invalidação.

- **Risco de escala** (pergunta do Manager): se um agente únicos dispara 100 req/s, são 100 DB hits/s de validação de token. Para PostgreSQL com índice cacheado, isto é trivial (<1% CPU). **Só vira problema com orders of magnitude a mais** → momento de adicionar Redis.

### Resolução: Alinhado. Sem cache positivo no MVP, interface abstraída para Redis futuro. O DevOps está correto em preparar a abstração; eu e o Backend estamos corretos em não usá-la no MVP.

---

## T3 — Secret Store: ESO vs Sealed Secrets vs Vault

### Posições
- **DevOps** (§3.6): ESO + cloud secret manager (nível 2), com Sealed Secrets (nível 1) como alternativa, Vault (nível 3) só se multi-cluster. Recomendação forte por ESO.
- **Eu** (Turno 1, §9.1): ordem de preferência External Secrets > Sealed Secrets > Vault direto. Deixei em aberto dependendo do ambiente alvo.
- **DBA** (§9.3): "K8s Secret ou Vault/External Secrets Operator" — sem prefêrencia explícita.

### Veredito: ESO como default, Sealed Secrets como fallback on-prem

**Alinhamento integral com o DevOps**. Reforço três pontos de segurança:

1. **KEK (master encryption key) e TOKEN_HASH_PEPPER** vivem no secret store externo, materializados como K8s Secret via ESO. Nunca em `values.yaml`, nunca bakeados em imagem.
2. **`encryption_key_id`** (DBA §2.7) é essencial para **rotação de KEK**. Recomendo:
   - MVP: KEK único `v1`, sem rotação automática.
   - ROADMAP: rotação anual, com re-encriptação lazy (novas chaves usam `v2`, antigas migradas por job).
3. **Anti-padrões proibidos** (alinhado com DevOps):
   - `Secret` Helm base64 versionado em Git.
   - Long-lived cloud credentials no GitHub Actions → usar **OIDC federation**.
   - Variáveis de ambiente com KEK em texto plano no `Deployment.yaml`.

### Pergunta em aberto para o DevOps
Para ambientes sem cloud secret manager (K8s on-prem sem AWS/GCP/Azure), a recomendação é Sealed Secrets ou Vault dev mode? **Minha opinião**: Sealed Secrets é mais simples e suficiente para 30 usuários; Vault dev mode adiciona operação sem retorno proporcional.

### Resolução: Alinhado. ESO + cloud backend (prod), Sealed Secrets (on-prem), `.env` gitignored (dev local).

---

## T4 — Ordem de middleware no Beego (confirmação de default-deny)

### Posições
- **Eu** (Turno 1, §6.2): middleware chain Recover → RequestID → Auth → RBAC → RateLimit → Audit. Default-deny via annotation explícita `@Public`.
- **Backend** (§10.1): pacote `auth/middleware.go` para Bearer → user.
- **Frontend** (§5): Beego Filter valida role antes de renderizar. Sessão separada da auth do proxy.
- **Software Architect** (§6.1): Auth Middleware valida Bearer → user.

### Veredito: Compatível, mas exige duas cadeias distintas (proxy vs admin)

A separação que o Frontend propõe (cookie/session para UI, Bearer para proxy) é **correta e necessária** do ponto de vista de segurança:

| Superfície | Auth | Middleware chain |
|---|---|---|
| `/v1/*` (proxy OpenAI-compat) | Bearer `sk-...` | Recover → RequestID → **BearerAuth** → TokenActive → RateLimit → Audit |
| `/admin/*` e `/api/v1/web/*` (UI admin + JSON p/ charts) | Sessão (cookie httpOnly) | Recover → RequestID → **SessionAuth** → CSRFCheck → RBAC → RateLimit → Audit |
| `/login`, `/healthz`, `/readyz`, `/version` | Pública | Recover → RequestID |

**Implementação Beego** (`web.InsertFilter`):

Beego tem constantes de posição: `BeforeStatic`, `BeforeRouter`, `BeforeExec`, `AfterExec`, `FinishRouter`. **Autenticação e RBAC DEVEM estar em `BeforeRouter`** — caso contrário, o roteador pode fazer match da rota antes da validação, expondo metadados (lista de rotas em logs de debug, erro 404 vs 401 distinguindo rotas existentes).

**Default-deny prático em Beego**:
```go
// Lista explícita de rotas públicas (opt-in)
publicRoutes := map[string]bool{
    "/login": true,
    "/healthz": true,
    "/readyz": true,
    "/version": true,
}

web.InsertFilter("/*", web.BeforeRouter, func(ctx *context.Context) {
    path := ctx.Request.URL.Path
    if publicRoutes[path] {
        return // allow
    }
    // Para rotas /v1/*: exigir Bearer
    // Para rotas /admin/* e /api/v1/web/*: exigir sessão + CSRF + RBAC
    // Caso contrário: 401
})
```

### Confirmação de alinhamento
- ✅ Default-deny é compatível com a proposta do Backend (auth/middleware.go).
- ✅ Separação session vs Bearer é compatível com o Frontend.
- ✅ RBAC enforcement em middleware (não no controller) é o padrão — Frontend está correto em "UI nunca é a barreira".
- ⚠️ **Atenção ao Frontend**: a menção "rotas admin protegidas por middleware que valida role" deve ser explícita: **toda mutação via HTMX passa por CSRF**, e **todo acesso a `/admin/*` passa por RBAC**, independentemente de o link aparecer na sidebar. Concordo plenamente com esta postura.

### Resolução: Alinhado. Cadeias distintas para proxy (Bearer) e admin (Session+CSRF+RBAC). Filters em `BeforeRouter`.

---

## T5 — Afinidade de cache de contexto: hash em memória viola política de "não logar conteúdo"?

### Contexto
- **Eu** (Turno 1, §10.3): "NUNCA logar conteúdo de mensagens. Afinidade deve detectar prefixo apenas em memória, sem persistir fingerprint."
- **Backend** (§5.4): SHA-256 truncado para 128 bits do prefixo, em memória com TTL 30 min, limite 50 conversas/usuário.
- **Software Architect** (ADR-006): rolling hash das primeiras K mensagens, in-memory, best-effort.
- **DevOps** (§7.1): "NUNCA logar payloads de request/response — apenas metadados."

### Veredito: Hash em memória é ACEITÁVEL, mas exige regra estrita sobre logs

**Análise de risco**:

1. **O hash NÃO é conteúdo**: é um fingerprint criptográfico. SHA-256 truncado a 128 bits não é reversível. Atacante que capture o hash não recupera o conteúdo.

2. **Persistência**:
   - Hash em memória com TTL 30min = **não persistido** → não viola "nunca persistir fingerprint".
   - Hash em disco/DB/Redis = **persistido** → **violaria** a política. **Proibido no MVP.**

3. **Risco residual**: se atacante compromete o processo (RCE, memory dump, heap snapshot):
   - Já tem acesso a TODO o conteúdo das mensagens em trânsito no processo — o hash é o menor dos problemas.
   - Hash permite **correlação** entre conversas (saber que dois requests pertencem à mesma conversa) — vazamento de metadado, não de conteúdo.
   - Mitigação: TTL curto (30 min) + limite de tamanho (50 conversas/usuário) limita o window de exposição.

4. **Preferência pela Opção 1 do Backend** (`X-Conversation-ID` header):
   - ID fornecido pelo cliente, não derivado de conteúdo.
   - **Zero fingerprint de conteúdo** no proxy.
   - Cliente gera o ID (UUID) e envia — proxy apenas usa como chave de cache.
   - **Esta é a opção preferida do ponto de vista de privacidade.**
   - Hash de prefixo fica como **fallback heurístico** apenas para clientes que não enviam o header.

### Regras estritas para o spec

1. **PROIBIDO**: logar (em qualquer nível: debug, info, error) o hash de afinidade, o `affinity_key`, ou qualquer fingerprint derivado de conteúdo de mensagem.
2. **PROIBIDO**: persistir o hash de afinidade em Redis, DB, arquivos de cache em disco, ou backups.
3. **PROIBIDO**: incluir o hash em métricas Prometheus (labels), traces OpenTelemetry (attributes), ou mensagens de erro.
4. **PERMITIDO**: usar o hash em memória para decisão de roteamento, descartando após uso ou TTL.
5. **RECOMENDADO**: priorizar `X-Conversation-ID` header quando presente; logar APENAS `conversation_id` (string opaca fornecida pelo cliente) em usage_logs quando existir, para correlação de auditoria — este ID não é derivado de conteúdo, é metadado explícito do cliente.

### Pergunta direta para o Backend
Confirma que o `affinity_key` (rolling hash) jamais aparece em logs/métricas/traces, mesmo em nível debug? **É não-negociável do ponto de vista de privacidade.** O `X-Conversation-ID` (quando presente) pode ser logado.

### Resolução: Alinhado, com a ressalva explícita acima. Hash em memória é aceitável; logging do hash é proibido.

---

## T6 — `organization_id` nullable vs NOT NULL: implicação de isolamento de tenant

### Posições divergentes
- **DBA** (§2.2): `NOT NULL DEFAULT 1` com seed org `id=1`. Justifica: custo de migração em `usage_logs` particionada é altíssimo.
- **Software Architect** (ADR-004): Opção A (NOT NULL DEFAULT default_org_id) recomendada; Opção B (nullable) como fallback.
- **Backend** (§13): "`tenant_id` (default null = 'default')". **Nullable.**
- **DevOps** (§10.3): "`tenant_id` nullable desde o início". **Nullable.**
- **Eu** (Turno 1, §6.1, §10.4): `tenant_id` nullable "para facilitar migração".

### Veredito: NOT NULL DEFAULT 1 é estritamente superior do ponto de vista de SEGURANÇA

Mudo minha posição do Turno 1. Após ver o argumento do DBA e do Software Architect, **adopto NOT NULL DEFAULT 1**. Razões de segurança:

1. **Nullable = vetor de leakage silencioso**:
   - Com nullable, queries que "esquecem" o filtro `WHERE organization_id = ?` retornam TODAS as linhas, incluindo `NULL`. Em MVP single-tenant isto é invisível (só NULL existe), mas no momento da migração multi-tenant, qualquer query esquecida vira **cross-tenant data leak** silencioso.
   - Exemplo trágico: `SELECT * FROM usage_logs WHERE user_id = ?` esquece `organization_id` → retorna logs de outros tenants se o `user_id` colidir (improvável, mas com IDs sequenciais BIGINT, IDs baixos existem em todos os tenants).

2. **NOT NULL DEFAULT 1 = invariant em schema**:
   - Toda linha tem `organization_id`. Queries sem filtro ainda retornam tudo — mas o multi-tenant data leak só acontece quando há dados de múltiplos tenants E a query não filtra.
   - A constraint NOT NULL força desenvolvedores a pensar no escopo (coluna visível com FK).
   - Adicionar checagem automatizada: repositório pattern exige `orgID` no contexto da requisição, panics se ausente.

3. **Defesa em profundidade adicional**: implementar **强制 filter** via pattern de repositório:

```go
type UsageRepo interface {
    // orgID é obrigatório — não há overload sem ele
    FindByUser(ctx context.Context, orgID, userID int64, filter Filter) ([]Usage, error)
}
```

Lint rule customizada (Semgrep) para detectar queries SQL sem `organization_id` em tabelas multi-tenant.

4. **Custo de migração** (argumento decisivo do DBA): adicionar coluna NOT NULL em `usage_logs` particionada com milhões de registros exige `ADD COLUMN ... DEFAULT ... NOT NULL` em múltiplos passos (add nullable → backfill → set NOT NULL VALIDATE). Em partição mensal, isto é operação longa com `ACCESS EXCLUSIVE` lock. **Fazer agora custa zero.**

### Recomendação FINAL
- **`organization_id BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id)`** em TODAS as tabelas multi-tenant (`users`, `api_tokens`, `providers`, `provider_keys`, `models`, `usage_logs`, `audit_log`, `rate_limits`, `model_aliases`, `model_pricing`, etc.).
- Seed: `INSERT INTO organizations (id, name, slug) VALUES (1, 'Default', 'default')`.
- Índices composite começando por `organization_id` em queries tenant-scoped.
- Repository pattern com `orgID` obrigatório no contexto.
- Lint check (Semgrep) para queries sem filtro de organização.

### Apelo direto ao Backend e DevOps
**Reconsiderem a posição de nullable.** O argumento de "facilitar migração" é invertido: nullable torna a migração MAIS arriscada (queries esquecidas viram leak silencioso), não mais fácil. NOT NULL DEFAULT 1 força disciplina desde o dia 1. Isto é uma decisão de segurança, não apenas de modelagem.

### Resolução: **DBA + Software Architect + Security convergem em NOT NULL DEFAULT 1**. Backend e DevOps devem alinhar.

---

## T7 — Divergências e pontos adicionais de segurança

### T7.1 — DevOps propõe 2 réplicas em prod, mas rate limit em memória

**Problema de segurança**: o DevOps reconhece (§9.1) que rate limit em memória + multi-réplica = limite efetivo `N × configurado`. Para o proxy, o rate limit é um **controle de segurança** (proteção contra DoS, abuso, exaustão de chaves), não apenas de governança.

- Se 2 réplicas, limite configurado de 60 req/min → efetivo 120 req/min. Um atacante com token roubado pode consumir o dobro do esperado.
- Para 30 usuários internos, o impacto é marginal. Mas para um **token vazado** (o vetor nº 1 de incidente), o dobro do consumo acelera o esgotamento de quotas.

**Recomendação**: 
- Para o MVP, **1 réplica em prod** é aceitável (alinhado com o Backend e o Software Architect, que assumem single-replica). HA pode esperar.
- **OU**: se HA é must-have, Redis-backed rate limit desde o MVP (interface abstraída do DevOps + implementação Redis).
- **NÃO** aceitar "2 réplicas + rate limit em memória" como solução estável — é uma dívida de segurança.

### T7.2 — Prefixo `sk-aipp-` para GitHub secret scanning

Nenhum par mencionou. **Reitero**: registrar o pattern `sk-aipp-[a-zA-Z0-9]{8}-[a-zA-Z0-9]{48}` no GitHub Secret Scanning + push protection. Isto faz commits que acidentalmente incluam tokens serem bloqueados automaticamente. **Custo zero, valor alto** — deve entrar como tarefa do spec, não como roadmap.

### T7.3 — Rate limit em endpoints admin (não só no proxy)

Nenhum par especificou rate limit para:
- `/login` (proteção brute-force) — eu propus 10/min/IP, 5/min/username.
- Criação de token (`POST /me/tokens`) — propus 5/hora/usuário (evita geração massiva para fugir de rastreamento).
- Mutações admin (`/admin/*` POST/PUT/DELETE) — propus 120/min/session.

**Estes devem entrar no spec explicitamente**, não como subproduto do rate limit do proxy.

### T7.4 — SSRF em `base_url` de provedores (reiteração)

Backend (§3.1), Software Architect (ADR-007), DevOps (§8.4 NetworkPolicy) tratam o fluxo de proxy, mas nenhum aborda explicitamente a **validação da `base_url` configurável por admin**. Esta é uma superfície de SSRF — mesmo admin confiável, defesa em profundidade contra SQLi → SSRF.

**Reitero a necessidade** de:
- Validação de scheme (`https` apenas em prod).
- Bloqueio de IPs: loopback, RFC1918, link-local (incluindo `169.254.169.254` metadata cloud), ULA IPv6.
- DNS rebinding defense: resolver hostname E conectar ao IP resolvido.
- NetworkPolicy K8s como layer adicional (DevOps já propôs).

### T7.5 — DBA: `last_used_ip INET` em `api_tokens`

Concordo. Mas há implicação de PII: IPs são dados pessoais sob LGPD. **Recomendação**:
- Hashear IP antes de persistir (`HMAC(ip, pepper)`) para correlação sem reter IP real. Permite detectar "mesmo IP usando múltiplos tokens" sem reter o IP.
- OU: truncar (`192.168.x.x`) — menos útil para correlação mas mais simples.
- Definir política de retenção (90 dias, alinhado com usage_logs).

### T7.6 — DevOps: `readOnlyRootFilesystem` + Beego

Concordo. Mas Beego pode tentar escrever em `./logs/` ou `./runtime/` por default. **Configurar Beego para stdout-only logging** e montar `emptyDir` em `/tmp` se necessário. Caso contrário, pod crasha em startup.

### T7.7 — Software Architect: graceful shutdown no MVP

Apoio integralmente puxar graceful shutdown do ROADMAP para o MVP. Sem ele, RollingUpdate corta streams SSE em andamento — para um produto de chat, isto é visível ao usuário e pode causar perda de dados de uso (logs não registrados se o pod morre abruptamente). Já mencionei no Turno 1.

### T7.8 — Frontend: server-rendered + HTMX

Totalmente alinhado do ponto de vista de segurança:
- Sessão httpOnly + SameSite=Strict: ✅
- CSRF token em mutações: ✅
- Renderização condicional por role no servidor (UI nunca é barreira): ✅
- Sem JWT em localStorage (evita XSS → token theft): ✅
- Separação Bearer (proxy) vs Session (UI): ✅

**Acesso**: "defesa em profundidade: verificação de role NÃO vive no frontend" — perfeito, exatamente minha postura.

---

## Síntese das decisões consolidadas (para o spec)

| # | Decisão | Convergência |
|---|---|---|
| C1 | **Tokens**: formato `sk-aipp-[keyid8]-[secret48]`, storage `HMAC-SHA256(secret, PEPPER)` com UNIQUE, lookup O(1), comparação constant-time. **Proibido Argon2id/bcrypt para tokens.** | Security + DBA (com adição de pepper) |
| C2 | **Cache de token no MVP**: DB hit por request (sem cache positivo). Interface `TokenValidator` abstraída para Redis futuro. | Security + Backend + DBA + DevOps |
| C3 | **Secret store**: ESO + cloud backend (prod), Sealed Secrets (on-prem), `.env` (dev). KEK + pepper + JWT secret + DB URL externos. | Security + DevOps + DBA |
| C4 | **Middleware**: duas cadeias (Bearer para `/v1/*`, Session+CSRF+RBAC para `/admin/*` e `/api/v1/web/*`). Filters em `BeforeRouter`. Default-deny com allowlist de rotas públicas. | Security + Backend + Frontend |
| C5 | **Afinidade de cache**: hash em memória com TTL 30min, limite 50 conv/user. Preferir `X-Conversation-ID` header. **Proibido logar/persistir hash de afinidade.** | Security + Backend (com ressalva de não-log) |
| C6 | **`organization_id`**: `BIGINT NOT NULL DEFAULT 1` em todas as tabelas multi-tenant. Seed org default. Repository pattern com `orgID` obrigatório. Lint Semgrep para queries sem filtro. | Security + DBA + Software Architect (Backend e DevOps devem alinhar) |
| C7 | **Réplicas em prod**: ou 1 réplica (MVP simples) ou Redis-backed rate limit desde o MVP. **Não aceitar 2 réplicas + rate limit em memória como solução estável.** | Security + DevOps (com ressalva) |
| C8 | **`sk-aipp-` prefix**: registrar no GitHub Secret Scanning + push protection. | Security (novo, sem par) |
| C9 | **Rate limits explícitos**: login (10/min/IP, 5/min/user), criação de token (5/h/user), admin mutações (120/min/session), proxy (60/min/token segurança + configurável/user negócio). | Security (complementa Backend) |
| C10 | **SSRF guard**: validar `base_url` (https-only, block private IPs, DNS rebinding defense) + NetworkPolicy egress. | Security (reiterado, complementa DevOps) |
| C11 | **IP em logs/tokens**: hashear com pepper ou truncar. Retenção 90 dias. | Security + DBA (com ajuste PII) |
| C12 | **Beego hardening**: stdout-only logging, `emptyDir` em `/tmp`, `readOnlyRootFilesystem`, `runAsNonRoot`, drop ALL caps. | Security + DevOps |
| C13 | **Graceful shutdown no MVP** (puxado do ROADMAP). | Security + Software Architect + DevOps |

---

## Perguntas finais para o Turno 3 / Consensus

1. **Backend**: confirma rejeição de Argon2id/bcrypt para tokens (adoção de HMAC-SHA256+pepper)?
2. **Backend**: confirma que hash de afinidade jamais aparece em logs/métricas/traces?
3. **Backend e DevOps**: aceitam migrar de `tenant_id nullable` para `organization_id NOT NULL DEFAULT 1`?
4. **DevOps**: prefere 1 réplica (MVP simples) ou Redis-backed rate limit desde o MVP (HA real)?
5. **Todos**: ninguém objetou ao prefixo `sk-aipp-` — confirmam adoção + registro no GitHub Secret Scanning?

---

## Resumo do delta

**Convergências principais**: envelope encryption (todos), ESO como secret store (Security + DevOps + DBA), separação Bearer/Session (Security + Frontend), graceful shutdown no MVP (Security + Software Architect + DevOps), argon2id para senhas (todos).

**Tensões resolvidas**:
- HMAC+pepper vence SHA-256 puro para tokens (proteção contra confirmation attack); Argon2id para tokens é rejeitado (quebra lookup, sem benefício para alta entropia).
- `organization_id NOT NULL DEFAULT 1` vence nullable (segurança contra leak silencioso cross-tenant).
- DB hit por request no MVP, Store abstraction para Redis futuro.

**Pontos que reitero sem par que tenha tocado**: prefixo `sk-aipp-` no GitHub Secret Scanning, rate limits explícitos em endpoints admin, SSRF guard em `base_url`, hashing de IPs em logs (PII/LGPD).
