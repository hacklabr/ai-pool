# Analysis: Security Architect — Turn 3

**Agent ID:** security-security-architect
**Timestamp:** 2026-06-25T19:22:23.941Z

# Posição de Consenso Final — Security Architect (4º, Turno de Consenso)

> Posiciono-me em relação aos 3 pares que falaram (Software Architect, Backend Architect, DevOps Engineer). Fecho as decisões de segurança do MVP.

---

## 1. Voto: Opção A (MVP single-réplica, Store abstrações desde já) ✅

**CONFIRMO Opção A.** Alinhado com os três pares anteriores.

### Justificativa de segurança (não apenas operacional)

A Opção A não é apenas "mais simples" — é **mais segura para o MVP** por uma razão específica que quero explicitar:

**Com 1 réplica + DB hit por request, a invalidação de token ao desabilitar usuário é trivialmente instantânea e atômica.** Não há cache em nenhum pod para invalidar, não há pub/sub para falhar, não há janela de race entre purge e repopulação. A próxima query ao DB após o `UPDATE users SET status='disabled'` vê o estado atualizado. Ponto.

Isto elimina uma **classe inteira de bugs de segurança** (cache invalidation) do MVP. Quando migrarmos para multi-réplica (Opção B futura), aí sim teremos que enfrentar a complexidade de invalidação distribuída — mas com a Store abstraction já no lugar (conforme DevOps propôs e todos concordamos), a migração é injetar `RedisValidator` no lugar de `DBValidator`.

**Confirmação direta à pergunta do Backend**: sim, com 1 réplica, DB hit por request resolve invalidação instantânea trivialmente. Sem ressalvas.

---

## 2. Cache de Token — Vitória 3-1, refirmo a posição técnica

O placar está 3-1 a favor de **DB hit por request puro** (Backend, DevOps, Security a favor; Software Architect defendendo cache 15s+purge).

### Por que DB hit puro é MAIS SEGURO que cache (não apenas mais simples)

Reafirmo os três pilares que tornam DB hit superior do ponto de vista de segurança:

1. **Revogação atômica e determinística**: ao desabilitar usuário, a próxima validação de token já vê o estado novo. Não há TTL, não há purge, não há janela. O teste de revogação é determinístico (não depende de timing).

2. **Zero modos de falha de invalidação**: cache+purge introduz 4 modos de falha reais que detectei no Turno 2 (purge silencioso, race de repopulação, multi-réplica sem propagação, testes não-determinísticos). DB hit elimina todos.

3. **Latência irrelevante no contexto**: com UNIQUE index em `key_id`, lookup é sub-ms. O LLM call domina (100ms–10s). O overhead de validação é ruído — mesmo a 100 req/s agregados (argumento do Software Architect), PostgreSQL não pisca.

### Resposta direta ao Software Architect

Ao Software Architect, respeito a preocupação com bursts de agente automatizado, mas o argumento é **mal endereçado por cache de token**:

- Se um agente legítimo dispara 100 req/s, isso é problema de **rate limiting de negócio** (que todos concordamos implementar: 60/min/user configurable), não de cache de validação.
- Cache de token reduz carga de **validação**, não de **throughput do proxy**. O bottleneck de um burst nunca é a validação do token — é o provedor de LLM.
- O custo de complexidade do purge (bugs potenciais, conforme Backend corretamente apontou) supera o benefício marginal em escala de 30 usuários.

**Aceito a derrota gracefully** caso o Software Architect insista — 15s window não é uma vulnerabilidade crítica (em sistemas OAuth é comum). Mas **técnica e seguramente, DB hit é a escolha superior para o MVP**. Recomendo que o Software Architect conceda este ponto; o consenso técnico é claro.

### Decisão fechada
- **MVP**: DB hit por request, zero cache positivo.
- **Interface `TokenValidator`** abstraída (alinhado com DevOps), com implementações `DBValidator` (ativa) e `RedisValidator` (futura, com cache ≤30s + pub/sub invalidation).
- **Gatilho de migração para cache**: profiling em produção mostrando connection pool saturation OU latência p99 de validação > 5ms OU > 1.000 usuários ativos.

---

## 3. Decisões de SEGURANÇA do MVP — Fechadas (minha responsabilidade)

Confirmo e fecho as seguintes decisões de segurança, todas com convergência entre os pares:

### 3.1 Criptografia em repouso das chaves de provedores
- **Envelope encryption**: AES-256-GCM com DEK por chave + KEK master externo.
- **KEK via ESO**, materializado como **arquivo montado readOnly** em `/etc/aipp-secrets/kek` (alinhado com DevOps Turno de Consenso).
- **`encryption_key_id`** coluna em `provider_keys` para rotação futura de KEK (DBA §2.7).
- **Decryption apenas no request path**, zeroizar `[]byte` após uso, nunca logar.

### 3.2 Tokens de usuários (`sk-...`)
- **Formato**: `sk-aipp-[keyid8]-[secret48]` — key_id público (lookup O(1)), secret hasheado.
- **Storage**: `token_hash = HMAC-SHA256(secret, TOKEN_HASH_PEPPER)` como BYTEA, UNIQUE.
- **Pepper**: 32 bytes, no mesmo secret store do KEK (ESO → arquivo ReadOnly).
- **Comparação**: `hmac.Equal` (constant-time), nunca `==`.
- **PROIBIDO**: Argon2id/bcrypt para tokens (Backend aceitou a correção no Turno 2).
- **Prefixo `sk-aipp-`**: registrar no **GitHub Secret Scanning + push protection**. Custo zero, deve estar no spec (não no roadmap).

### 3.3 Senhas de login
- **Argon2id** (memory=64MB, iterations=3, parallelism=2) via `golang.org/x/crypto/argon2`.
- Política: mínimo 12 caracteres, NIST 800-63B (sem restrições pedantes).
- Coluna `failed_login_count` em `users` (DBA §2.1) para lockout progressivo.

### 3.4 RBAC e middleware
- **Default-deny**: rotas privadas por padrão, allowlist explícita de públicas (`/login`, `/healthz`, `/readyz`, `/version`).
- **Duas cadeias de middleware** (alinhado com Frontend):
  - `/v1/*` (proxy): Bearer `sk-...` → `TokenActive` → RateLimit → Audit.
  - `/admin/*` e `/api/v1/web/*`: Session (cookie httpOnly+Secure+SameSite=Strict) → CSRFCheck → RBAC → RateLimit → Audit.
- **Filters em `BeforeRouter`** no Beego (não `BeforeExec`) — garante que roteador não expõe metadados antes da auth.
- **RBAC enforcement no servidor**, nunca no cliente (UI nunca é barreira — Frontend alinhado).

### 3.5 Rate limiting como controle de segurança
Limites explícitos (além do rate limit de negócio configurável por usuário no proxy):
- **`/login`**: 10/min/IP, 5/min/username (anti brute-force).
- **Criação de token** (`POST /me/tokens`): 5/hora/usuário (anti geração massiva).
- **Admin mutações** (`/admin/*` POST/PUT/DELETE): 120/min/session.
- **Proxy** (`/v1/*`): 60/min/token (segurança) + configurável/user (negócio).
- **Implementação MVP**: in-memory token bucket (`golang.org/x/time/rate`). Com 1 réplica, consistência garantida.

### 3.6 SSRF guard em `base_url` de provedores
- Validação de scheme: `https` apenas em produção.
- **Blocklist de IPs**: loopback (`127.0.0.0/8`, `::1`), RFC1918 (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), link-local (`169.254.0.0/16` — inclui metadata cloud `169.254.169.254`), ULA IPv6 (`fc00::/7`), link-local IPv6 (`fe80::/10`).
- **DNS rebinding defense**: resolver hostname E conectar ao IP resolvido (não re-resolver no cliente HTTP).
- **NetworkPolicy K8s** como layer adicional (DevOps alinhado: egress 443 + DNS).

### 3.7 SanitizingLogger
- Logger customizado que varre campos conhecidos (`authorization`, `api_key`, `x-api-key`, `bearer`, `x-api-key`) e aplica redação.
- Hook no `logs` package do Beego e no `slog` estruturado (Backend/DevOps alinhados em `slog`).
- **Nunca** usar `httputil.DumpRequestOut`/`DumpResponse` em produção.
- Panic recovery: mensagem genérica `{"error":"internal_error"}`, stack trace só em log interno.

### 3.8 Política de logging de conteúdo
- **NUNCA logar conteúdo de mensagens** (request/response bodies). Default não-negociável.
- **Hash de afinidade** (rolling hash do prefixo): **APENAS em memória**, TTL 30min, limite 50 conv/user. **PROIBIDO** logar/persistir em Redis/DB/disco/métricas/traces.
- **`X-Conversation-ID`** header (quando presente): pode ser logado (metadado opaco do cliente, não fingerprint de conteúdo).
- **PII em logs**: IPs hasheados com pepper OU truncados (`192.168.x.x`). Email nunca em logs de uso.

### 3.9 Audit log
- Tabela `audit_log` append-only (DBA §2.11).
- **Grants PostgreSQL**: `GRANT INSERT ON audit_log TO aipp_app;` (sem UPDATE/DELETE) — imutabilidade em nível de DB.
- Captura: login, role change, key mgmt (create/disable/enable/rotate), token mgmt, config changes, rate-limit hits.
- Retenção: 1 ano (vs 90 dias para usage_logs).

### 3.10 LGPD/GDPR preparation (barato agora)
- **`organization_id BIGINT NOT NULL DEFAULT 1`** em todas as tabelas multi-tenant (ver §4 abaixo).
- **IPs hasheados** em logs e `api_tokens.last_used_ip`.
- **Endpoints `/me/export` e `DELETE /me`** (soft delete + purge job) no roadmap — schema já preparado com `deleted_at`.
- **Política de retenção documentada** em `SECURITY.md` ou `PRIVACY.md`.
- **Prefixo `sk-aipp-`** registrado no GitHub Secret Scanning (facilita detecção de vazamento e "direito ao esquecimento" — tokens vazados podem ser identificados e rotacionados).

### 3.11 Hardening de infraestrutura (K8s)
- `runAsNonRoot: true`, `readOnlyRootFilesystem: true`, `capabilities.drop: ["ALL"]`, `seccompProfile: RuntimeDefault`.
- Distroless `base-debian12:nonroot` (precisa de CA certs para HTTPS — DevOps correto em rejeitar `static`).
- `emptyDir` em `/tmp` (Beego pode escrever temporários).
- Beego configurado para **stdout-only logging** (sem `./logs/` em disco).
- Resources limits/requests sempre.

---

## 4. `organization_id` NOT NULL DEFAULT 1 ✅

**Confirmo converged**. Mudei de posição no Turno 2 (era nullable no Turno 1) após argumentos do DBA e do Software Architect.

- `organization_id BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id)` em todas as tabelas multi-tenant.
- Seed: `INSERT INTO organizations (id, name, slug) VALUES (1, 'Default', 'default')`.
- Índices composite começando por `organization_id` em queries tenant-scoped.
- **Repository pattern com `orgID` obrigatório** no contexto (sem overload sem orgID).
- **Lint Semgrep** para detectar queries SQL sem filtro de organização em tabelas multi-tenant.

**Apelo final ao Backend e DevOps** (que propuseram nullable no Turno 1): alinhem-se. NOT NULL DEFAULT 1 é estritamente superior em segurança (evita leak silencioso cross-tenant quando queries esquecem o filtro na migração multi-tenant). O DBA e o Software Architect já estão alinhados. A convergência é 4-2 a favor de NOT NULL (assumindo que Backend e DevOps cedem).

---

## 5. Pontos de discordância com o consenso

**Nenhum ponto de discordância técnica significativo.** O consenso é forte.

Único ponto que registro como **observação, não objeção**: o DevOps menciona rede interna do cluster como "trusted" (plain HTTP entre Ingress e Service, §8.3 do Turno 1). Para o MVP single-tenant com 30 usuários internos, isto é aceitável. **Mas no roadmap**, quando multi-tenancy chegar, mTLS interno (Istio/Linkerd) deve ser considerado — um pod comprometido em namespace compartilhado não deveria sniffar tokens Bearer em trânsito interno. Registro como dívida técnica, não como bloqueador.

---

## 6. Resumo do placar de consenso (da perspectiva de segurança)

| Decisão | Posição final | Convergência |
|---|---|---|
| Opção arquitetural | **A** (single-réplica, Store abstrações) | 4-0 |
| Cache de token | **DB hit por request** (zero cache positivo no MVP) | 3-1 (Software Architect deve conceder) |
| `organization_id` | **NOT NULL DEFAULT 1** | 4-2 (Backend e DevOps devem alinhar) |
| Envelope encryption | **AES-256-GCM + KEK via ESO (arquivo ReadOnly)** | 5-0 |
| Tokens | **HMAC-SHA256+pepper, `sk-aipp-...`** | 5-0 (Backend aceitou correção Argon2id) |
| Senhas | **Argon2id** | 5-0 |
| RBAC | **Default-deny, duas cadeias (Bearer/Session)** | 5-0 |
| Rate limits | **Login + token creation + admin + proxy** | 5-0 |
| SSRF guard | **Em app + NetworkPolicy** | 5-0 |
| Logging de conteúdo | **Proibido por default, hash afinidade só em memória** | 5-0 |
| Audit log | **Append-only via DB grants** | 5-0 |
| IP hashing | **Pepper ou truncamento** | 5-0 (reforço do DBA) |
| GitHub Secret Scanning | **Registrar `sk-aipp-` pattern** | Security isolado (deve entrar no spec) |
| Graceful shutdown | **No MVP** | 5-0 |

---

## 7. Declaração final

**Consensus reached** do ponto de vista de segurança. Todas as decisões críticas de segurança do MVP estão fechadas com convergência majoritária ou unânime entre os especialistas. Os dois pontos em disputa residual (cache de token e `organization_id` nullable) têm convergência técnica clara a favor da posição mais segura; aguardo concessão formal do Software Architect (cache) e do Backend/DevOps (nullable).

Estou pronto para a fase de SPECIFICATION. As decisões de segurança estão prontas para serem traduzidas em requisitos verificáveis e testes de segurança automatizados no pipeline CI (SAST com Semgrep/GoSec, secrets scanning com Gitleaks, dependency audit com Trivy/govulncheck — todos já propostos pelo DevOps no Turno 1 §6.1).
