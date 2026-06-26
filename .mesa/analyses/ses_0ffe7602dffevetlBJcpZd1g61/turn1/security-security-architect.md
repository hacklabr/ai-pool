# Análise de Segurança — AI Provider Pool (Turno 1)

> Perspectiva: Security Architect. Foco em modelo de ameaças, proteção dos ativos sensíveis (chaves de provedores, tokens de usuários, credenciais de login), controles preventivos e preparação para conformidade/LGPD.

---

## 1. Visão Executiva de Risco

Este sistema é um **honeypot de alto valor**. Ao centralizar chaves de múltiplos provedores de LLM (Gemini, Mistral, DeepSeek, Z.ai, Kimi), ele concentra em um único alvo aquilo que hoje está pulverizado e informalmente compartilhado via Telegram. **A motivação para construir é segurança; a consequência é que uma falha aqui é pior do que o problema atual.**

Três classes de ativos críticos, em ordem de criticidade:

| Ativo | Impacto se vazado | Observabilidade do vazamento |
|---|---|---|
| **Chaves de provedores** (5 provedores × até 5 chaves = ~25 chaves) | Crítico — fraude financeira, abuso ilimitado, roubo de quota | Baixa — uso indevido parece legítimo |
| **Tokens de usuários (sk-...)** | Alto — impersonificação de usuários para consumo do pool | Média — aparece em logs de uso |
| **Senhas de login (admin)** | Alto — acesso admin = acesso a tudo | Alta — mas dano já feito |
| **Conteúdo de mensagens** (se logado) | Alto/Regulatório — PII, segredos de negócio | Alta se vazar LGPD |

**Princípio arquitetural recomendado**: tratar o sistema como uma **cofre (vault) de chaves com API controlada**, não como "um proxy que por acaso guarda chaves". Essa mudança de mentalidade orienta todas as decisões abaixo.

---

## 2. Modelo de Ameaças (STRIDE + Trust Boundaries)

### 2.1 Trust Boundaries

```
┌─────────────────────────────────────────────────────────────┐
│ TB-0: Internet (hostil)                                      │
└──────────────────────────┬──────────────────────────────────┘
                           │ TLS 1.3, WAF, Rate Limit
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ TB-1: Proxy App (container Go/Beego)                         │
│   • Validar token sk-... do cliente                          │
│   • Verificar usuário ativo                                  │
│   • Rate limit de segurança + negócio                        │
└───────┬─────────────────────────────────┬───────────────────┘
        │                                 │
        ▼                                 ▼
┌────────────────────────┐    ┌──────────────────────────────┐
│ TB-2: PostgreSQL       │    │ TB-3: Provedores de LLM      │
│  • TLS obrigatório     │    │  • TLS verify (nunca skip)   │
│  • Credenciais mínimas │    │  • Egress allowlist          │
│  • Chaves cifradas     │    │  • Timeouts curtos           │
└────────────────────────┘    └──────────────────────────────┘
```

### 2.2 STRIDE — Análise Prioritária

| # | Threat | Componente | Risco | Cenário de Ataque | Mitigação Primária |
|---|--------|-----------|-------|-------------------|-------------------|
| S1 | **Spoofing** | Token sk-... roubado | Alto | Vazamento em log de app cliente, repo público, screenshot | Hash em DB + revogação + curto TTL de cache de validade |
| S2 | **Spoofing** | Brute-force de senha admin | Médio | Ataques automatizados em `/login` | Rate limit + lockout + argon2id + (futuro) MFA |
| T1 | **Tampering** | Manipulação de request no proxy | Médio | Cliente tenta injetar headers para alterar roteamento/modelo | Allowlist de headers repassados; nunca repassar `Authorization` do cliente |
| T2 | **Tampering** | Mass assignment em API admin | Médio | `{"role":"admin"}` em endpoint de update de perfil | DTOs explícitos, nunca bind direto para model |
| R1 | **Repudiation** | Usuário nega uso abusivo | Médio | "Não fui eu que consumi 1M de tokens" | Log imutável de uso (hash chaining opcional) |
| I1 | **Info Disclosure** | Chave de provedor em log/erro | **Crítico** | Stack trace ou mensagem de erro do provider exibe Authorization | Sanitização centralizada de logs; masks; nunca logar header outbound |
| I2 | **Info Disclosure** | Conteúdo de mensagens em logs | Alto | Mensagens contêm PII/segredos e vazam em storage de logs | **Default: NÃO logar conteúdo**; opção opt-in cifrada |
| I3 | **Info Disclosure** | Enumeration de usuários | Baixo | `/login` diferencia "usuário inexistente" de "senha errada" | Mensagens genéricas, timing constante |
| I4 | **Info Disclosure** | IDOR na gestão de tokens | Médio | Usuário A lista tokens do usuário B via `/api/users/{id}/tokens` | Validação `token.user_id == authenticated_user_id OR is_admin` |
| D1 | **DoS** | Exaustão de chaves por quota | Médio | Atacante dispara requisições para esgotar quota das chaves | Rate limit por usuário + auto-disable inteligente |
| D2 | **DoS** | Slowloris em streaming SSE | Médio | Cliente abre conexão SSE e nunca consome | Timeout de leitura, max conexões por token/IP |
| D3 | **DoS** | Algoritmo complexo no proxy | Médio | JSON muito profundo, payload gigante | Limites de tamanho/depth; rejeitar antes de processar |
| E1 | **Elevation of Privilege** | Bypass RBAC no proxy | **Crítico** | Bug no middleware permite consumir pool sem token | Middleware obrigatório por default; opt-out explícito |
| E2 | **Elevation of Privilege** | JWT/session role tampering | Alto | Adversário altera claim de role | Sessions server-side (não JWT stateless) ou JWT assinado com chave rotativa |
| E3 | **SSRF** | URL de provedor configurável | Alto | Admin malicioso (ou via SQLi) configura URL = `http://169.254.169.254/...` | Allowlist de hostnames; bloquear RFC1918, loopback, link-local, metadata |

---

## 3. Ativo #1 — Chaves de Provedores (o coração do sistema)

### 3.1 Classificação e princípios

Estas chaves são **críticas e de alto valor**. Devem ser protegidas como segredos de produção, não como dados operacionais comuns.

**Regras inegociáveis:**
1. **Nunca** persistir em texto plano (nem em DB, nem em config, nem em backups não criptografados).
2. **Nunca** aparecer em logs (app, acesso, erro, debug, trace distribuído).
3. **Nunca** ser retornada por nenhuma API — nem mesmo para admin (apenas máscara `sk-...****AB12`).
4. **Nunca** ser enviada para o cliente em mensagens de erro.
5. Tempo máximo em memória: o da requisição. **Descartar** após uso (zeroizar slice em Go).

### 3.2 Criptografia em repouso — Envelope Encryption

**Esquema recomendado (camadas):**

```
[ Chave provedor (plaintext) ]
            │ AES-256-GCM com DEK (Data Encryption Key, por registro)
            ▼
[ Chave provedor (ciphertext) ] ──► armazenada em coluna `encrypted_secret` no PostgreSQL
            │
[ DEK (ciphertext) ] ──► armazenada junto (ou em coluna separada)
            │ AES-256-GCM com KEK (Key Encryption Key, master)
            ▼
[ KEK ] ──► NUNCA no banco; vive em:
            • K8s: Sealed Secret / External Secrets Operator / Vault
            • Cloud: KMS (AWS KMS, GCP KMS, Azure KV)
            • Dev local: arquivo `.env` fora do repo + instrução clara
```

**Por que envelope encryption** (e não só AES direto com KEK):
- Cada chave de provedor tem DEK próprio → rotação de chave individual sem re-criptografar tudo.
- KEK nunca toca o banco → vazamento do dump do DB não expõe chaves (apenas ciphertext inútil sem KEK).
- Performance: criptografia simétrica AES-GCM é rápida; KEK (assimétrico/KMS) usado raramente.
- Rotação de KEK: re-encripta apenas os DEKs, não as chaves dos provedores.

**Biblioteca Go recomendada**: `google/tink` (abstrai AES-GCM, KMS integration, audited) ou `aws/aws-sdk-go-v2/service/kms`. **Proibido**: implementação própria de criptografia.

### 3.3 Ciclo de vida da chave em memória (request path)

```
1. Request chega → middleware valida token sk-... do cliente
2. Router escolhe provedor/chave (LRU + afinidade)
3. Query DB: SELECT encrypted_secret, dek_encrypted WHERE key_id = ?
4. Buscar KEK do secret store (cache em memória com TTL curto, 5-15 min)
5. Desencriptar DEK, depois desencriptar chave do provedor
6. Montar requisição ao provedor com `Authorization: Bearer <plaintext>`
7. APÓS envio: zeroizar slices que contêm plaintext (fill with zeros)
8. Em caso de erro: logar apenas status + sanitized message, nunca body/headers
```

**Crítico para Go**: usar `[]byte` (mutável) e não `string` (imutável, permanece em heap até GC). Para chaves: `defer zeroize(&keyBytes)`.

### 3.4 Exposição acidental — defesas profundas

- **Logger customizado**: implementar `SanitizingLogger` que varre campos conhecidos (`authorization`, `api_key`, `x-api-key`, `bearer`) e aplica redação. Beego usa `logs` package — hook nele.
- **HTTP client/dump**: garantir `httputil.DumpRequestOut` e `DumpResponse` **nunca** são usados em produção com logs habilitados. Configurar `http.Transport.TLSClientConfig.InsecureSkipVerify = false` sempre.
- **Mensagens de erro do provedor**: o briefing menciona parse de mensagens de erro. **Atenção**: algumas mensagens de erro podem conter echoes de Authorization em bodies de erro mal-formados de provedores. Aplicar regex de redação na resposta antes de logar.
- **Panic recovery**: Beego `recover` middleware deve capturar panic e gerar mensagem genérica (`{"error":"internal_error"}`), nunca stack trace no body. Stack trace só em log interno.
- **Mock/Dev**: modo `dev` com chaves FAKE explícitas; bloquear start em produção se KEK não configurada.

### 3.5 Rotação de chaves de provedores

- API admin: endpoint `POST /admin/provider-keys/{id}/rotate` que recebe a nova chave (sem jamais expor a antiga) e a re-encripta.
- Histórico: manter registros anteriores marcados como `superseded` (não deletar imediatamente — útil para auditoria), com TTL de retenção.
- Provedor-side: quando o provedor rotaciona (raro), admin apenas atualiza; nada muda arquiteturalmente.

### 3.6 Backups

- **Backup do PostgreSQL deve ser criptografado** (pg_dump + gpg, ou snapshots criptografados do PVC).
- KEK **nunca** entra no backup — fica separada no secret store.
- Restaurar backup sem KEK = dado inutilizável (propriedade desejada).

---

## 4. Ativo #2 — Tokens de Usuário (sk-...)

### 4.1 Geração criptograficamente segura

**Formato recomendado** (compatível com OpenAI, fácil rotação, lookup O(1)):

```
sk-aipp-[keyid8]-[secret48]
└─┘ └──┘ └──────┘ └──────┘
prefixo  projeto  key ID   segredo (base62, ~285 bits entropia)
```

- `keyid8`: identificador público, 8 chars base62 (~47 bits). Permite lookup O(1) no banco pelo índice.
- `secret48`: 48 chars base62 (~285 bits de entropia) gerados via `crypto/rand`.
- **Entropia efetiva**: ~285 bits no segredo → inviável brute-force mesmo com milhões de tentativas/seg.

**Por que separar keyid do secret**: permite armazenar apenas o **hash do secret** e ainda fazer lookup eficiente pelo `keyid` (índice não-sensível).

### 4.2 Armazenamento — nunca plaintext

**Esquema recomendado**:

```sql
CREATE TABLE user_api_tokens (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users(id),
    key_id        CHAR(8) NOT NULL UNIQUE,         -- parte pública, indexada
    secret_hash   BYTEA NOT NULL,                   -- HMAC-SHA256 do secret
    name          VARCHAR(100),                     -- rótulo humano ("meu-token-dev")
    last_used_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,                      -- NULL = sem expiração (desencorajar)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at    TIMESTAMPTZ,                      -- soft delete para auditoria
    CONSTRAINT active_unique UNIQUE (user_id, key_id) WHERE revoked_at IS NULL
);
CREATE INDEX idx_tokens_keyid_active ON user_api_tokens(key_id) WHERE revoked_at IS NULL;
```

- **HMAC-SHA256** com pepper secreto (em KEK/secret store) em vez de SHA-256 puro → protege contra rainbow tables mesmo se DB vazar.
- Retorno do token legível **apenas uma vez** no momento da criação. Posteriormente, sistema só conhece o hash.
- Exibição na UI: `sk-aipp-AB12CD34-••••••••` (máscara fixa).

### 4.3 Validação em request path (lookup O(1))

```go
// Pseudocódigo
func ValidateToken(raw string) (*User, error) {
    prefix, keyID, secret, ok := parseToken(raw)
    if !ok || prefix != "sk-aipp" { return nil, ErrInvalid }
    
    cached, hit := tokenCache.Get(keyID)         // L1: in-memory, TTL 30s
    if hit {
        return cached.user, hmac.Equal(cached.hmac, hmacSHA256(secret))
    }
    
    tok, err := db.FindActiveTokenByKeyID(keyID) // L2: DB
    if err != nil || tok == nil { return nil, ErrInvalid }
    
    expected := hmacSHA256WithPepper(secret)
    if !hmac.Equal(tok.SecretHash, expected) { return nil, ErrInvalid }
    
    if tok.RevokedAt != nil || tok.User.DisabledAt != nil { 
        return nil, ErrRevoked 
    }
    if tok.ExpiresAt != nil && time.Now().After(*tok.ExpiresAt) {
        return nil, ErrExpired
    }
    
    tokenCache.Set(keyID, cachedEntry{...}, ttl=30*time.Second)
    return tok.User, nil
}
```

- `hmac.Equal` é **constant-time** — evita timing attacks.
- Cache de **validade positiva** com TTL curto (30s). Em L2 ir ao DB (que tem índice em `key_id`).
- **Cache negativo**: pequeno (10s) para tokens inválidos reduz load em scans.

### 4.4 Revogação imediata (requisito crítico do briefing)

**Problema central**: quando admin desabilita/exclui usuário, o briefing exige **perda imediata de acesso**. Com cache de 30s, há janela de até 30s onde tokens continuam válidos. Solução em camadas:

1. **TTL curto no cache** (30s) — já reduz janela.
2. **Invalidação ativa**: ao desabilitar usuário, **limpar todas as entradas de cache** daquele usuário em todas as réplicas:
   - Para MVP single-pod: `tokenCache.PurgeUser(userID)` direto.
   - Para múltiplas réplicas: pub/sub em Redis (`DEL user:{id}:tokens`), ou usar Redis como camada de cache compartilhada.
3. **Flag ativa em cache**: armazenar no cache entry um `user_disabled=false`. Se a flag inverter (via invalidação), cache é descartado.
4. **Defense-in-depth**: periodicamente (a cada 1 min), background job re-confere `users.disabled_at IS NULL` para entradas em cache.
5. **Garantia real de "instantâneo"**: se PODEROSO requisito, **não fazer cache positivo** e ir ao DB em cada request. Com índice em `key_id`, é query de ~1ms. Para 30 usuários é trivial. **Recomendo esta opção para MVP** pela simplicidade e segurança garantida.

> **Decisão recomendada**: MVP sem cache positivo (DB hit por request, query indexada), adicionando cache Redis em roadmap quando escala exigir.

### 4.5 Expiração e rotação

- Permitir `expires_at` opcional ao criar token.
- UI encoraja expiração (default: 90 dias).
- Job diário purge tokens expirados > N dias.
- Usuário pode ter múltiplos tokens (rotação sem downtime: criar novo, atualizar clientes, revogar velho).

---

## 5. Ativo #3 — Autenticação de Login (web admin & users)

### 5.1 Senhas

- **Hash**: `argon2id` (preferido) via `golang.org/x/crypto/argon2`. Parâmetros recomendados: `memory=64MB, iterations=3, parallelism=2`. Alternativa aceitável: `bcrypt` cost ≥ 12.
- **Nunca** MD5, SHA-x direto, ou PBKDF1.
- Política de senha: mínimo 12 caracteres, sem restrição pedante de caracteres (NIST 800-63B). Considerar check contra lista de senhas vazadas (HIBP API ou dump local) — barato e eficaz.
- Em signup/admin-create: senha enviada via HTTPS, hasheada imediatamente, **nunca logada**.

### 5.2 Brute-force / account lockout

- Rate limit por **IP** + por **username** (ambos) em `/login`: ex., 10 tentativas/min/IP, 5 falhas/min/username.
- **Lockout progressivo**: após 5 falhas para mesmo username, delays crescentes (1s, 2s, 4s, 8s, ...).
- **Não revelar** se username existe: mensagem genérica `"credenciais inválidas"` com timing constante (mesmo tempo para usuário inexistente).
- Considerar CAPTCHA após N falhas (roadmap).

### 5.3 Sessão / cookies

**Recomendação**: **server-side sessions** (não JWT stateless para admin web).

- Motivo: revogação imediata ao desabilitar usuário é trivial (deletar session do store). JWT stateless torna revogação custosa (denylist).
- Store: Redis (produção) ou tabela `sessions` no PostgreSQL (MVP).
- Cookie: `Name=session; HttpOnly; Secure; SameSite=Strict; Path=/; MaxAge=8h`.
- **Rotation**: gerar novo session ID após login bem-sucedido (evita session fixation).
- **CSRF**: 
  - Para endpoints server-rendered com cookie: token CSRF (double-submit cookie ou sincronizado em header `X-CSRF-Token`).
  - Para API consumida via token sk-...: CSRF não aplicável (Bearer header não é enviado automaticamente pelo browser). **Manter API de proxy 100% Bearer** para simplificar.
- Logout: invalidar session server-side + limpar cookie.

### 5.4 MFA (roadmap, mas preparar)

- Esquema de DB já com coluna `mfa_secret_encrypted` (nullable) e `mfa_enabled` em `users`.
- Quando `mfa_enabled=true`, login exige segundo passo: `POST /login/verify-totp`.
- Biblioteca: `pquerna/otp` (TOTP compatível Google Authenticator).
- **Backup codes** hasheados como senhas.

### 5.5 OAuth (roadmap)

- Deixar abstração `AuthProvider` desde o início: interface `Authenticate(credentials) (User, error)` com implementações `PasswordAuth`, futura `OAuthAuth`.
- Tabela `user_identities` (provider, subject, user_id) já desenhada — facilita migração.

---

## 6. RBAC e Enforcement

### 6.1 Modelo de roles

Para MVP: **2 roles** (`admin`, `user`). Já modelar para escalabilidade:

```sql
CREATE TABLE roles (id SERIAL, name VARCHAR(50), ...); -- admin, user
CREATE TABLE users (id BIGSERIAL, ..., role_id INT NOT NULL REFERENCES roles(id), ...);
-- Roadmap: 
-- CREATE TABLE teams (id, name, ...);
-- CREATE TABLE team_members (team_id, user_id, role_in_team);
-- CREATE TABLE team_provider_access (team_id, provider_key_id);
```

**Para o roadmap de tenancy/teams**, recomendo **já adicionar coluna `tenant_id` (nullable)** em todas as tabelas relevantes (`users`, `provider_keys`, `tokens`), com default NULL agora. Migrar para NOT NULL depois é muito mais fácil do que adicionar coluna com milhões de registros.

### 6.2 Enforcement — middleware obrigatório

- **Default-deny**: todos os endpoints são privados por padrão; `@Public` annotation explícita para abrir (apenas `/login`, `/healthz`).
- **Middleware chain** (Beego filters):
  1. `RecoverFilter` — captura panic, retorna 500 genérico.
  2. `RequestIDFilter` — gera correlation ID, propaga em logs.
  3. `AuthFilter` — valida Bearer (proxy) OU session (admin web).
  4. `RBACFilter` — verifica role requerida pela rota.
  5. `RateLimitFilter` — limites por token/IP/rota.
  6. `AuditFilter` — loga evento de segurança (login, role change, key mgmt).
- **Beego**: usar `web.InsertFilter("*", web.BeforeRouter, ...)` para garantir ordem.

### 6.3 Testes de RBAC (security tests mandatórios)

Casos de teste que DEVEM existir no pipeline:
- Usuário comum acessando `/admin/*` → 403.
- Usuário desabilitado fazendo request → 401.
- Token revogado fazendo request → 401.
- Token expirado fazendo request → 401.
- Usuário A listando tokens do usuário B → 403 (IDOR).
- Usuário A atualizando próprio `role_id` para admin → 400/403 (mass assignment).
- Token sk-... malformado → 401 (não crash).

### 6.4 Princípio do menor privilégio no DB

- **Dois usuários PostgreSQL**: `aipp_app` (read/write em tabelas operacionais) e `aipp_migrations` (DDL). Migrações rodam só em deploy.
- `aipp_app` NÃO tem `DROP`, `TRUNCATE`, nem acesso a `pg_authid`.
- KEK (master) **não** vive no PostgreSQL — vive em secret store externo.

---

## 7. Rate Limiting como Controle de Segurança

### 7.1 Duas camadas distintas

| Camada | Escopo | Algoritmo | Propósito |
|---|---|---|---|
| **Segurança** | Por IP (global) + por token | Token bucket ou sliding window | Proteger infra; prevenir DoS, scanning, brute-force |
| **Negócio** | Por usuário (somando tokens) | Sliding window | Quota/uso justo (roadmap: configurável por usuário) |

### 7.2 Implementação

- **MVP**: in-memory com `golang.org/x/time/rate` (token bucket por IP/token). Reinicia a cada restart — aceitável para MVP.
- **Produção multi-réplica**: Redis com `INCR + EXPIRE` (sliding window) ou algoritmo GCRA.
- Limites sugeridos (ajustáveis):
  - Login: 10/min/IP, 5/min/username.
  - Criação de token: 5/hora/usuário.
  - Proxy API: 60/min/token (segurança) + configurable por usuário (negócio).
  - Admin API: 120/min/session.

### 7.3 Respostas distintas

- 429 com header `Retry-After`.
- **Não** revelar o limite exato no body (info disclosure).
- Logar evento de rate limit hit (sinal de abuso).

### 7.4 Rate limit é também controle anti-exaustão de chaves

Importante: o rate limit **protege as chaves do provedor** contra esgotamento intencional. Sem ele, um usuário malicioso (ou token vazado) pode drenar a quota de todas as chaves rapidamente. **Os limites por usuário devem ser compatíveis com a quota agregada dos provedores.**

---

## 8. Segurança do Proxy (request forwarding)

### 8.1 Headers — sanitize obrigatório

Antes de repassar ao provedor:
- **REMOVER**: `Authorization` (substituir pela chave do provedor), `Cookie`, `X-Forwarded-*` (exceto se explícito), qualquer header customizado do cliente suspeito.
- **DEFINE**: `Authorization: Bearer <provider-key>`, `User-Agent` (override configurável por provedor), `Content-Type` se necessário.
- **Allowlist positiva** de headers repassados (default-deny), não blocklist.

### 8.2 SSRF (atenção especial!)

O briefing permite URL do provedor configurável por admin. Mesmo admin confiável, defesa em profundidade contra SQLi → SSRF:

```go
func ValidateProviderURL(rawURL string) error {
    u, err := url.Parse(rawURL)
    if err != nil || (u.Scheme != "https") {  // http proibido em prod
        return ErrInvalidScheme
    }
    if u.Port() == "" { u.Port = "443" }
    ips, err := net.LookupIP(u.Hostname())
    if err != nil { return ErrDNS }
    for _, ip := range ips {
        if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || isMetadata(ip) {
            return ErrBlockedIP
        }
    }
    // Re-resolver a cada request (defesa contra DNS rebinding)
    return nil
}
```

- Bloquear: `127.0.0.0/8`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `169.254.0.0/16` (inclui `169.254.169.254` metadata cloud), `::1`, `fc00::/7`, `fe80::/10`.
- **DNS rebinding**: resolver hostname E conectar ao IP resolvido (não re-resolver dentro do cliente HTTP).
- **NetworkPolicy K8s**: egress restrito a IPs/domínios dos provedores — layer adicional.
- **Permitlist de hostnames de provedores** conhecidos (configurável mas com defaults rígidos).

### 8.3 Tamanho e tempo

- `MaxHeaderBytes` (1MB default).
- `MaxBytesReader` no body (ex.: 10MB para chat, configurável).
- Timeouts: `ClientTimeout=120s` para chat com streaming; `DialTimeout=5s`; `TLSHandshakeTimeout=5s`; `ResponseHeaderTimeout=30s`.
- **Idle connections**: limite no `http.Transport.MaxIdleConnsPerHost`.
- Para SSE streaming: `ReadTimeout=0` na conexão de stream, mas com `Server.ReadHeaderTimeout=10s` e middleware de `max connections per token`.

### 8.4 Validação de input

- O proxy **não** deve validar estritamente o schema OpenAI (quebra compatibilidade). Mas DEVE:
  - Limitar tamanho do body (defesa DoS).
  - Limitar profundidade JSON (defesa parser bomb) — `json.NewDecoder` com `DisallowUnknownFields()` é opcional, pode quebrar; melhor só limitar bytes.
  - Rejeitar `Content-Type` não-JSON no proxy.
- Para API admin: validação estrita via Beego ORM ou DTOs Go explícitos.

### 8.5 Streaming SSE — cuidados

- SSE mantém conexão aberta por minutos. Vulnerabilidades:
  - **Slowloris reverso**: cliente nunca lê → buffer de servidor enche. Mitigação: `http.Flusher` + `SetReadDeadline` no cliente.
  - **Resource leak**: goroutine por stream. Mitigação: limite de streams simultâneos por token + context cancel no fim.
  - **Buffer de erro**: mensagens de erro do provedor não devem incluir Authorization.

---

## 9. Segurança da Infraestrutura (Kubernetes + Helm)

### 9.1 Secrets

**NÃO** armazenar KEK, senhas de DB, pepper HMAC em:
- `values.yaml` (mesmo criptografado) — exposed em `helm get values`.
- Variáveis de ambiente em texto plano no Deployment YAML.
- Imagem Docker.

**Recomendado (em ordem de preferência)**:
1. **External Secrets Operator + Vault/AWS Secrets Manager/GCP Secret Manager** — gold standard.
2. **Sealed Secrets (Bitnami)** — OK para K8s-only setups; secrets cifrados no Git.
3. **Cloud KMS para KEK + secret mount como arquivo** (não env var) em volume readOnly.

**Deployment spec**:
```yaml
containers:
- name: aipp
  securityContext:
    runAsNonRoot: true
    runAsUser: 10001
    readOnlyRootFilesystem: true
    allowPrivilegeEscalation: false
    capabilities: { drop: ["ALL"] }
    seccompProfile: { type: RuntimeDefault }
  env:
  - name: KEK_PATH
    value: /etc/aipp-secrets/kek  # arquivo montado via secret volume
  volumeMounts:
  - name: secrets
    mountPath: /etc/aipp-secrets
    readOnly: true
podSecurityContext:
  fsGroup: 10001
volumes:
- name: secrets
  secret: { secretName: aipp-master-key, defaultMode: 0400 }
```

### 9.2 NetworkPolicy (egress allowlist)

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
spec:
  policyTypes: [Egress]
  egress:
  - to: [{ namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kube-system } } }]
    ports: [{ port: 53, protocol: UDP }]  # DNS
  - to: [{ ipBlock: { cidr: 10.0.0.0/8 } }]  # PostgreSQL interno
    ports: [{ port: 5432 }]
  - to: [{ namespaceSelector: {} }]  # Redis interno
    ports: [{ port: 6379 }]
  # Egress para provedores deve ser NOVO namespace ou allowlist por IP (difícil: IPs variam)
  # Solução: usar um egress proxy/Squid com allowlist de domínios.
```

> **Observação prática**: allowlist por domínio em NetworkPolicy nativa não existe. Para controle fino de egress por hostname, usar **Istio Egress Gateway** ou **Squid proxy** com ACL de domínios. No MVP, NetworkPolicy por IP do PostgreSQL + Redis é suficiente; egress para provedores fica aberto (mas com SSRF guard inside-app).

### 9.3 Pod Security Admission

- Namespace com label `pod-security.kubernetes.io/enforce: restricted`.
- Proibe privileged, hostNetwork, root, capabilities desnecessárias.

### 9.4 Recursos e probes

- `resources.limits` e `resources.requests` sempre (evita noisy neighbor + DoS por exaustão).
- `readinessProbe` em `/healthz/ready` (DB conectivo).
- `livenessProbe` em `/healthz/live` (processo vivo, sem DB).
- `startupProbe` (Beego pode demorar a subir em cold start).

### 9.5 Imagem

- **Distroless** ou `golang:alpine` final stage (multi-stage build).
- `USER nonroot` no Dockerfile (não usar root).
- Scan de vulnerabilidades no CI (Trivy).
- Reproducible builds: pinar versões em `go.sum` + Dockerfile (`FROM golang:1.22.4-alpine3.20`, não `latest`).

---

## 10. Conformidade, Auditoria e LGPD

### 10.1 O que logar (segregação de logs)

**Dois streams distintos**:

| Stream | Conteúdo | Retenção | Local |
|---|---|---|---|
| **Audit log** (segurança) | login, role change, key mgmt, token mgmt, config changes, rate-limit hits | 1 ano (imutável) | DB tabela `audit_events` ou append-only store |
| **Usage log** (negócio) | request_id, user_id, token_id (hash), provider, model, key_id, tokens_in, tokens_out, latency_ms, status_code | 90 dias | DB tabela `usage_events` |
| **App log** (debug) | erros com stack trace sanitizado, métricas internas | 30 dias | stdout (K8s logs) |
| **Message content** | **NUNCA por padrão** | — | — |

### 10.2 PII em logs

- **IP do cliente**: hashear antes de logar (HMAC com pepper) — permite correlação sem reter IP real. Ou truncar (`192.168.x.x`).
- **Email de usuário**: não logar em eventos de uso; apenas em audit.
- **Conteúdo de mensagens**: **PROIBIDO por padrão**. Justificativas:
  1. PII/segredos de negócio podem estar nas mensagens.
  2. Volume de storage inviável.
  3. Conformidade LGPD/GDPR (direito ao esquecimento mais difícil).
- Se futuramente desejado (debug de afinidade de cache), **opt-in por usuário**, cifrado, retenção curta (7 dias), com endpoint de purge.

### 10.3 Afinidade de cache — privacidade

A feature de **detecção de prefixo de conversa** tem implicação de privacidade:
- Se armazenarmos o hash do prefixo em cache (para roteamento futuro), estamos retendo fingerprint de conteúdo.
- **Recomendação**: detectar prefixo **apenas em memória por request** (comparar com cache em memória de últimas N mensagens do usuário), sem persistir. Cache em memória com TTL curto (5 min), keyed por `user_id + task_signature`.
- Documentar esta decisão no ROADMAP como escolha deliberada de privacidade.

### 10.4 LGPD/GDPR — o que é barato fazer AGORA

Mesmo com MVP single-tenant, preparar para LGPD futura:

1. **Schema com `tenant_id`** (nullable) em tabelas — já mencionado.
2. **Tabela `audit_events`** com `actor_id`, `action`, `target_type`, `target_id`, `metadata` (JSONB), `created_at`. Habilita "quem acessou o quê".
3. **Endpoint de export de dados do usuário** (`GET /me/export`) — retorna tokens (sem secret), usage logs do usuário. Custo: baixo, valor: alto.
4. **Endpoint de deleção** (`DELETE /me`) — soft delete + purge em background após 30 dias. Permite "direito ao esquecimento".
5. **Política de retenção** documentada em `SECURITY.md` ou `PRIVACY.md`.
6. **Consentimento**: não aplicável a MVP interno, mas modelar `consents` table para futuro.

### 10.5 Integridade do audit log

- Inserts append-only (sem UPDATE/DELETE nas tabelas de audit) — via grants PostgreSQL: `GRANT INSERT ON audit_events TO aipp_app;` (sem UPDATE/DELETE).
- Opcional (alto valor): hash chaining (`hash = SHA256(prev_hash || event_data)`) — detecta tampering.
- Backup do audit log separado e imutável (S3 Object Lock, WORM).

---

## 11. Ameaças Específicas a Considerar

### 11.1 Token sk-... vazado em cliente (maior risco realístico)

- Maioria dos vazamentos de tokens de API acontece no cliente (`.env` commitado, log de app, screenshot).
- Mitigações:
  - **Detecção de uso anômalo** (roadmap): location change, volume spike → alertar usuário.
  - **Rate limit por token** limita dano.
  - **UI mostra "último uso" e "IP de origem"** → usuário percebe uso indevido.
  - **Revogação self-service** fácil (1 clique).
  - **Prefixo `sk-aipp-` permite GitHub secret scanning** (registrar no GitHub como padrão → scanners de push bloqueiam commits).

### 11.2 Admin comprometido = jogo perdido

- Se admin é comprometido, atacante vê máscaras de chaves (não chaves) e pode desabilitar/habilitar keys, mas **não pode extrair chaves plaintext** (sistema nunca as expõe).
- Porém pode **rotacionar chaves para uma URL maliciosa** (SSRF / captura). Mitigação: 2FA para admin + allowlist de URLs de provedores + alertas de mudança de config.
- **Rotacionar KEK** (master) periodicamente — limita dano de vazamento do KEK.

### 11.3 SQL Injection no admin filters

- Beego ORM (`orm.NewOrm()`) usa parameterized queries por baixo — **SE** usar QueryBuilder/ORM. **PROIBIDO** `orm.Raw()` com concatenação de strings.
- Validar todos os inputs de filtro (`?provider=X&status=Y`) com allowlist.

### 11.4 Timing attack na comparação de token

- Já mencionado: `hmac.Equal` (constant-time) — não `==`.
- Para login: timing similar para usuário existente/inexistente (inclui hash fake).

### 11.5 Race condition na criação de usuário

- TOCTOU em "verifica se username existe, depois cria" → condição de corrida cria duplicatas.
- Mitigação: `UNIQUE constraint` no DB + tratar erro de constraint no app.

### 11.6 Logs de streaming com buffer em memória

- Cuidado: buffer de resposta do provedor pode conter Authorization em caso de erro. Sempre redact antes de logar.

### 11.7 Reenvio de mensagem para chave após failover

- Quando failover acontece, a mesma mensagem é reenviada para outra chave. Implicações:
  - Mensagens podem chegar múltiplas vezes a provedores diferentes.
  - Privacy: a mensagem foi exposta a mais de um provedor (já esperado, mas documentar).
- Sem ação de segurança específica, apenas transparência.

---

## 12. Decisões Recomendadas (Resumo)

| # | Decisão | Justificativa |
|---|---------|---------------|
| D1 | **Envelope encryption** com AES-256-GCM + DEK por chave + KEK externo | Protege chaves mesmo com dump do DB |
| D2 | **KEK em External Secrets/Vault/Sealed Secrets**, nunca no DB | Reduz blast radius |
| D3 | Tokens sk-... com `key_id` público + HMAC-SHA256(pepper) do secret | Lookup O(1) + proteção contra rainbow |
| D4 | **MVP sem cache positivo de token** (DB hit por request) | Revogação instantânea garantida; escala OK para 30 usuários |
| D5 | Server-side sessions para admin web (não JWT) | Revogação trivial |
| D6 | `argon2id` para senhas | Resistente a GPU/ASIC |
| D7 | Rate limit em 2 camadas (segurança por IP/token + negócio por usuário) | Anti-DoS + proteção de quota |
| D8 | Allowlist positiva de headers repassados + SSRF guard por IP blocklist | Defesa em profundidade |
| D9 | **NUNCA logar conteúdo de mensagens** (default), opt-in cifrado para debug | LGPD-ready + reduz volume |
| D10 | `SanitizingLogger` central para todo log | Prevenir vazamento acidental de chaves |
| D11 | `tenant_id` (nullable) em todas as tabelas desde já | Migração multi-tenancy trivial |
| D12 | `audit_events` append-only + (opcional) hash chaining | Repudiation defense |
| D13 | Prefixo `sk-aipp-` + registro no GitHub secret scanning | Detecção de vazamento em commits |
| D14 | `runAsNonRoot` + `readOnlyRootFilesystem` + drop ALL caps | Hardening K8s |
| D15 | NetworkPolicy egress restrito (pelo menos DB/Redis) | Lateral movement defense |
| D16 | DTOs explícitos para admin API (nunca bind para model) | Anti mass-assignment |

---

## 13. Perguntas em Aberto para Discussão com Pares

1. **Para o Backend Architect**: a ausência de cache positivo de token (D4) é aceitável? Para 30 usuários é trivial, mas se houver pico de requisições (ex.: agente fazendo 100 req/s), o DB vai sofrer. Como modelar cache com invalidação confiável (Redis pub/sub)?
2. **Para o Backend/Infra**: preferem External Secrets Operator ou Sealed Secrets para o KEK? Depende do ambiente alvo (cloud vs on-prem K8s).
3. **Para o Backend**: Beego tem suporte nativo a middleware order? Como garantir que `AuthFilter` roda sempre antes do handler?
4. **Para o DevOps/Infra**: há ambiente cloud específico (AWS/GCP/on-prem) que determine escolha de KMS vs Sealed Secrets?

---

## 14. Itens para o ROADMAP.md (Segurança)

- MFA TOTP para admins.
- Detecção de uso anômalo de token (location, volume).
- Cache Redis de validade de token com pub/sub invalidation.
- NetworkPolicy com egress proxy (Squid/Istio) para allowlist de provedores.
- GitHub secret scanning registration para `sk-aipp-` pattern.
- Endpoint `/metrics` Prometheus com métricas de segurança (auth_failures, rate_limit_hits, key_disables).
- Health check periódico de chaves com alertas.
- Backup automatizado e testes de restore (incluindo restore do KEK).
- Bug bounty / disclosure policy (`SECURITY.md`).
- Rotação automática de KEK anual.
- SBOM generation no CI (Syft).
- SAST no CI (Semgrep, GoSec).
- Dependabot/Renovate para dependências Go.

---

**Conclusão**: O sistema é viável e seguro **se** as decisões D1-D16 forem adotadas. O maior risco é tratá-lo como "um proxy simples" e não como "um cofre de chaves com API". A diferença de mentalidade orienta desde o schema do DB até a configuração do pod. A escala pequena (30 usuários) permite priorizar **correza de controles** sobre **performance de lookup** — revogação instantânea via DB hit é luxo acessível agora.
