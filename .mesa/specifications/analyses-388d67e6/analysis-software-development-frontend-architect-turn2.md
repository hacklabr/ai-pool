# Analysis: Frontend Architect — Turn 2

**Agent ID:** software-development-frontend-architect
**Timestamp:** 2026-06-25T18:56:31.980Z

# Delta — Frontend Architect (Turno 2)

## TL;DR
**Nenhum par desafiou minha recomendação server-rendered.** Software Architect e Backend Architect a endossam explicitamente e pedem minha confirmação. Há **5 ajustes concretos** à minha análise Turn 1 e **2 divergências alheias** (Security vs DBA sobre formato/hash do token) que afetam a UI e preciso sinalizar.

---

## 1. Convergências que reforçam server-rendered

### 1.1. Software Architect (ADR-007 + §9) — confirma UI admin ainda usa Beego
> "Beego cuida exclusivamente de: admin CRUD, dashboard, auth de admin [...] SPA é overkill para ~30 usuários; trade-off a confirmar com frontend specialist."

**Confirmação direta**: a UI admin ainda usa Beego mesmo com o Proxy Core isolado como handler HTTP puro (`/v1/*` fora do Controller Beego). A separação é por rota, não por processo — ambos convivem no mesmo binário. Para o frontend:
- Templates admin/dashboard são servidos por Controllers Beego (`/admin/*`, `/me/*`, `/api/v1/web/*` JSON).
- O proxy `/v1/*` não emite HTML — é puro repasse HTTP/SSE. A UI nunca renderiza nada do proxy; ela só consulta *dados sobre* o proxy.
- **Caminho quente e UI admin não compartilham controllers** — zero risco de buffering Beego quebrar o dashboard (dashboard não faz streaming).

Arquitetura CSS Turn 1 permanece 100% válida.

### 1.2. Backend Architect (§14.3) — responde às duas perguntas diretas
> "Dashboard server-rendered (Beego templates) vs SPA? Impacta arquitetura de admin REST. Como o admin vê 'qual chave está em cooldown até quando'?"

**P1 — Server-rendered vs SPA**: confirmado. **Não há "arquitetura de admin REST" no sentido SPA** — os endpoints admin são controllers Beego que retornam HTML (fragmentos HTMX) para mutações, e JSON **só** para os endpoints de gráfico do dashboard. O backend não precisa construir uma "API REST admin completa" — apenas controllers MVC com renderização de partials. **Isto reduz drasticamente o trabalho do backend** (sem serialização JSON para CRUD, sem DTOs para cada tela, sem OpenAPI spec para admin).

**P2 — "Qual chave está em cooldown até quando"**: tela mais complexa do produto (Turn 1 §2.6). O DBA modelou perfeitamente: `provider_keys.status` (`enabled`/`disabled`/`auto_disabled`), `disabled_reason` (`manual`/`quota_exceeded`/`rate_limited`/`error_threshold`/`auth_failed`/`parse_failed_retry`), `auto_reactivate_at TIMESTAMPTZ`. A UI consome esses campos diretamente em partial HTMX:

```html
<tr class="c-key-row c-key-row--{{.Status}}">
  <td class="c-key-row__label">{{.Label}}</td>
  <td class="c-key-row__status">
    {{template "partials/keys/_status-badge.tpl" .}}
    {{if eq .Status "auto_disabled"}}
      <span class="c-key-row__meta">↻ em {{timeUntil .AutoReactivateAt}}</span>
    {{end}}
  </td>
  <td class="c-key-row__actions">
    {{if eq .Status "enabled"}}
      <button hx-post="/admin/keys/{{.ID}}/disable" class="c-button c-button--ghost">Desabilitar</button>
    {{else if eq .Status "auto_disabled"}}
      <button hx-post="/admin/keys/{{.ID}}/force-enable" class="c-button c-button--warning">Forçar reativação</button>
    {{end}}
  </td>
</tr>
```

`c-status-badge` com modifiers BEM cobre os 3 estados. `disabled_reason` vira tooltip/aria-label. Contagem regressiva é texto server-rendered ou `<time>` element com JS mínimo. Opcionalmente `hx-trigger="every 60s"` no container para refresh de estados de reativação.

### 1.3. DevOps (§3.4) — path routing compatível
> "Path routing: API em `/`, dashboard em `/dashboard` (ou subdomínio)."

Confirmado. UI em `/admin/*` + `/me/*`, proxy em `/v1/*`. Ingress roteia por path prefix. **Sem conflito.**

---

## 2. Ajustes concretos à análise Turn 1 (5 correções)

### 2.1. ⚠️ Formato do token — alinhar com Security Architect
Na Turn 1 escrevi máscara genérica `sk-abc...`. **Security (§4.1)** propõe formato estruturado: `sk-aipp-[keyid8]-[secret48]` com prefixo `sk-aipp-` **registrável no GitHub secret scanning** — defesa gratuita contra vazamento em commits. **DBA (§2.3)** usa `sk-...` genérico. **Divergência alheia que afeta a UI**.

**Minha posição**: adoto Security. Razões de UI:
1. Prefixo `sk-aipp-` é idiot-proof — usuário sabe de onde vem o token (vs confusão com OpenAI real).
2. `keyid8` público permite identificador estável na UI (não sensível), enquanto `secret48` é mascarado após criação.
3. GitHub secret scanning é gratuito e bloqueia commits acidentais.

**Máscara exibida**: `sk-aipp-AB12CD34-••••••••`. Na criação: completo **uma vez** com botão copiar + banner de aviso. **Componente `c-token-display` com dois modos**: `--full` (criação) e `--masked` (default).

**Ação para spec**: alinhar formato canônico com Backend/Security.

### 2.2. ⚠️ Hash do token — divergência Security vs DBA (sinalizar)
- **Security (§4.2)**: `HMAC-SHA256(secret)` com pepper.
- **DBA (§2.3)**: `SHA-256(token)` puro.

**Não é decisão minha**, mas para UI é indiferente (nunca vejo o hash). **Sinalizo para Manager resolver**. Recomendação: o que importa é lookup O(1) (ambos via índice) e que a UI nunca reenvia o token para lookup (o cliente LLM envia Bearer, o proxy resolve; a UI só cria/revoga).

### 2.3. ➕ NOVA tela: Auditoria (admin) — não estava no mapeamento Turn 1
**Security (§10.1)** separa dois streams: **audit log** (login, role change, key mgmt, token mgmt, rate-limit hits; retenção 1 ano; append-only) e **usage log** (negócio; 90 dias). **DBA (§2.11)** modela `audit_log(id, organization_id, actor_user_id, action, entity_type, entity_id, before JSONB, after JSONB, ip, user_agent, created_at)`.

**Adicionar tela de Auditoria**:
```
Sidebar (admin):
  - Dashboard, Provedores, Chaves, Usuários
  - Auditoria  ← NOVO
  - Tokens (admin próprios), Meu uso
```

Tela: tabela `c-data-table` com colunas (timestamp, ator, ação, entidade, IP), filtros por ator/ação/período (HTMX), paginação server-side, **read-only** (append-only no DB). Mostra diff `before`/`after` em modal expandível (JSON formatado). Reutiliza componentes já propostos. **Custo baixo, valor alto** — auditoria é parte do driver "governança" nº 1 do briefing.

### 2.4. ➕ NOVO KPI: "Tokens economizados por cache de contexto"
**DBA (§2.9, §3.4)**: coluna `cached_tokens` em `usage_logs` mede o benefício da afinidade — **o diferencial declarado do produto** (Briefing objetivo 4).

**Adicionar ao dashboard como KPI destacado**:

| KPI Card | Valor |
|---|---|
| Total de requisições | N (período) |
| Tokens consumidos | prompt + completion |
| **Tokens economizados (cache)** | **cached_tokens** — destacar como diferencial |
| Taxa de erro | N% |
| Chaves ativas / desabilitadas | N / M |

Merece **destaque visual** (`c-stat-card--success` ou `--featured`) porque comunica o valor do produto. Pode virar gráfico de linha temporal "economia acumulada" — narrativa forte para adoção interna. **Custo trivial** se o backend popular `cached_tokens` no log.

### 2.5. ➕ Fluxo de login multi-step (preparar MFA agora, mesmo no roadmap)
**Security (§5.4, §5.5)** prepara schema para MFA (`mfa_secret_encrypted`, `mfa_enabled`) e fluxo `POST /login/verify-totp`. Prepara `AuthProvider` interface + `user_identities` para OAuth futuro.

**Fluxo de login multi-step desde o MVP**, mesmo só step 1 (senha) ativo. Adicionar MFA depois num fluxo single-step força redesign; adicionar num fluxo já multi-step é só ativar step 2.

**Ajuste ao `c-login-form`** (Turn 1 §2.2):
```
Estado 1: senha (MVP ativo)
  [username] [password] [Entrar]
  ─────────────────────────────
  (slot futuro: [Entrar com Google/OAuth])

Estado 2: MFA (roadmap, desenhado mas oculto)
  [código TOTP de 6 dígitos] [Verificar] ← Voltar

Sucesso: HX-Redirect → /dashboard (ou /me/usage)
```

Alpine controla estado `step: 1 | 2`. **Slot OAuth** (`<slot name="alt-auth">`) no rodapé, vazio no MVP, recebe botão quando OAuth chegar. **Custo agora: ~1h de design. Economia futura: redesign evitado.**

---

## 3. Contrato de API para dashboard — consumir rollups, não logs

### 3.1. Proposta DBA (§3.4)
Duas tabelas de rollup (preferidas a MATERIALIZED VIEW por upsert incremental):
- `usage_rollups_hourly(organization_id, user_id, provider_id, model_name, bucket_hour, request_count, prompt_tokens, completion_tokens, total_tokens, error_count, avg_latency_ms, ...)`
- `usage_rollups_daily(... bucket_date ...)`

Job (pg_cron ou app) agrega última hora fechada via `INSERT ... ON CONFLICT DO UPDATE` (idempotente). **Dashboard consulta somente rollups → queries de ms**.

### 3.2. Refinamento do endpoint JSON (Turn 1 §3)
Endpoint **deve consultar rollups, nunca logs crus**:

```
GET /api/v1/web/dashboard/usage
    ?from=2026-06-01&to=2026-06-25
    &granularity=hour|day      ← hora p/ ≤7d, dia p/ maiores
    &metric=tokens|requests|errors|cached_tokens|latency|cost
    &group_by=user|provider|model

→ 200 OK
{
  "granularity": "day", "metric": "tokens", "group_by": "user",
  "series": [{"label":"joao","data":[120,340,...]}, ...],
  "buckets": ["2026-06-01", ...],
  "totals": {"joao": 12450, ...},
  "currency": "USD"
}
```

**Decisões alinhadas com DBA**:
- `granularity` escolhe a **tabela de rollup** (`hour` → horária; `day` → diária). **Nunca agrega em runtime a partir de `usage_logs`** — se rollup ainda não materializou (até 1h atraso), endpoint retorna o que tem + `"partial": true`.
- `metric` mapeia para coluna no rollup.
- `cost` retorna NULL para todo MVP (coluna `cost_usd` nullable existe mas não populada). UI esconde seletor "cost" até roadmap.
- `group_by` é dimensão de agregação. Para `/me/usage`, backend filtra `user_id = session.user_id` antes de agrupar.

### 3.3. ⚠️ Gap sinalizado ao DBA: rollups precisam de `cached_tokens`
DBA (§3.4) lista rollups com `request_count, prompt_tokens, completion_tokens, total_tokens, error_count, avg_latency_ms`. **Falta `cached_tokens`**. Como proponho (§2.4) um KPI de tokens economizados, o rollup precisa materializar essa soma. **Solicito ao DBA adicionar `cached_tokens BIGINT` ao rollup horário e diário**. Custo: uma coluna a mais no job de agregação.

### 3.4. Cache de endpoint — ajuste para 300s
Turn 1 propus 60s. Como dados vêm de rollup (atualizado a cada hora pelo job), 60s é conservador. **Ajusto para 300s (5min)** — frescura máxima do dado é 1h anyway.

### 3.5. Biblioteca de charting — reconfirmo Apache ECharts
Sem mudança. Lazy-load só em `/dashboard` e `/me/usage`.

---

## 4. Auth/sessão — confirmação de compatibilidade

### 4.1. Alinhamento total com Security Architect
**Security (§5.3, D5)**: server-side sessions (não JWT stateless), store em Redis (prod) ou tabela `sessions` PG (MVP). Cookie `HttpOnly; Secure; SameSite=Strict; Path=/; MaxAge=8h`. Rotation após login. CSRF double-submit ou `X-CSRF-Token` header. Para proxy API: 100% Bearer. **Isto é exatamente minha proposta Turn 1 §5**. Confirmação total.

### 4.2. Sem conflito de middlewares — AuthFilter ramifica
**Security (§6.2)** propõe `AuthFilter` que valida Bearer (proxy) OU session (admin web). Confirmo compatibilidade:

```
AuthFilter logic:
  if route starts with /v1/*:
    validate Bearer (sk-aipp-...)
    on fail → 401 JSON
  elif route starts with /admin/*, /me/*, /api/v1/web/*:
    validate session cookie
    on fail → 302 redirect to /login?next=<original>
    on success → inject user + role in context
  elif route == /login or /healthz:
    pass through (public)
```

RBACFilter depois verifica role para `/admin/*`. **UI nunca é barreira.**

### 4.3. CSRF — implementação concreta
- Beego middleware injeta CSRF token em `<meta name="csrf-token">` no `<head>` de todo template autenticado.
- `<body hx-headers='{"X-CSRFToken": "{{.CSRFToken}}"}'>` — HTMX envia automaticamente em todo `hx-post`/`hx-put`/`hx-delete`.
- Server valida header contra session-bound token. Mismatch → 403.
- **HTMX torna isto trivial** — principal vantagem sobre SPA, onde CSRF com cookies exige configuração manual por request.

### 4.4. Expiração de sessão — UX
HTMX `htmx:responseError` handler global (Alpine) captura 401/302-to-login:
1. Toast `c-toast--warning`: "Sua sessão expirou. Faça login novamente."
2. Redireciona para `/login?next=<current_path>` preservando contexto.

Barato, evita anti-padrão "usuário preencheu form, perdeu tudo".

---

## 5. embed.FS + readOnlyRootFilesystem — confirmação DevOps

### 5.1. Compatibilidade confirmada
**DevOps (§3.2)**: `readOnlyRootFilesystem: true` + `emptyDir` em `/tmp`. **Minha proposta Turn 1 §7**: assets via `//go:embed`.

**Sem conflito**: assets embedded **não são escritos no filesystem** — são compilados no binário, lidos da memória. `readOnlyRootFilesystem: true` é totalmente compatível. `emptyDir` em `/tmp` cobre Beego temp files. **Confirmação ao DevOps**: binário único com embed.FS, Helm chart com 1 Deployment, compatível com securityContext restrictivo. ✅

### 5.2. Hot reload em dev — confirmação
**DevOps (§2.2)** menciona `air` para hot reload Go. Para templates/CSS: Beego em `RunMode=dev` serve do disco (`views/`, `web/static/`), não do embed.FS. Mudanças em `.tpl`/`.css` refletidas sem rebuild. **Compatível**. JS vendor commito no repo — em dev também servidos do disco.

### 5.3. Annotations de Ingress — impacto zero na UI admin
**DevOps (§3.4)**: `nginx.ingress.kubernetes.io/proxy-buffering: off` mandatório para SSE do proxy. Para UI admin (HTML/CSS/JS, não streaming), buffering off é **indiferente** (arquivos pequenos, sem chunked). **Sem ajuste necessário**.

---

## 6. Multi-tenant na UI — absorvendo `organization_id NOT NULL DEFAULT 1`

### 6.1. Decisão DBA vs Software Architect — convergem em NOT NULL DEFAULT 1
- **Software Architect (ADR-004)**: Opção A — `tenant_id NOT NULL DEFAULT <default_org_id>`.
- **DBA (§2.2)**: ainda mais forte — `organizations(id=1, slug='default')` seed + `organization_id BIGINT NOT NULL DEFAULT 1` + **composite index começando por `organization_id`**.

**Ambos convergem**: NOT NULL DEFAULT 1 desde o MVP. DBA mais rigoroso (índices composite).

### 6.2. Impacto na UI — 3 preparações concretas agora

#### 6.2.1. Middleware injeta `organization_id` no contexto
```go
func OrgFilter(ctx *context.Context) {
    user := ctx.Input.GetData("user").(*domain.User)
    ctx.Input.SetData("organization_id", user.OrganizationID)
    ctx.Input.SetData("current_org", orgService.Get(orgID))
}
```
Todo controller admin lê `c.Data["organization_id"]` e filtra queries. MVP: sempre `1`. Futuro multi-tenant: valor vem do seletor no topbar.

#### 6.2.2. Topbar com slot para seletor de organização
```html
<header class="c-topbar">
  <div class="c-topbar__brand">AI Provider Pool</div>
  <div class="c-topbar__org-selector">
    <span class="c-org-badge">{{.CurrentOrg.Name}}</span>
  </div>
  <div class="c-topbar__actions">...</div>
</header>
```
MVP: `c-org-badge` mostra "Default" (estático). Futuro: vira `<select>` Alpine com `hx-get` que re-renderiza com `org_id` no contexto. **Custo agora: 1 partial com slot. Economia futura: redesign do topbar evitado.**

#### 6.2.3. Rotas com namespace futuro
- `/admin/dashboard`, `/admin/providers`, `/admin/keys`, `/admin/users`, `/admin/audit` (implicitamente escopadas por `organization_id` da sessão)
- `/me/tokens`, `/me/usage`, `/me/profile`

Futuro multi-tenant: se URLs explícitas, adicionar prefixo `/org/:slug/admin/*` sem mudar controllers. Se implícito (cookie/seletor), zero mudança de URL.

### 6.3. Quotas e limites de horário — componentes futuros já previstos
**DBA (§6.1)** já cria `quotas` e `time_window_limits` vazias. Quando chegarem:
- **Quota panel** (admin): componente `c-quota-bar` (progress bar). Tokens `--color-warning` (80%) e `--color-danger` (95%) já definidos no design system Turn 1 §4.
- **Time window config**: form por usuário — reutiliza `c-form-field` + timepicker.
- **Notification de quota**: toast `c-toast--warning` quando API retorna 429 com `Retry-After`.

**Nenhuma feature exige novo componente arquitetural** — só novos dados alimentando componentes existentes. Items do roadmap = "popular dados", não "redesignar UI".

---

## 7. Divergências alheias que afetam a UI — sinalizar ao Manager

### 7.1. Formato do token `sk-...` (Security vs DBA) — tratado em §2.1
**Minha posição**: adoto Security (`sk-aipp-AB12CD34-••••`). Peço confirmação no spec.

### 7.2. Hash do token (Security HMAC-SHA256+pepper vs DBA SHA-256 puro) — tratado em §2.2
**Minha posição**: indiferente para UI. Manager decide com Security/DBA.

### 7.3. Sessions store (Security: Redis prod / tabela `sessions` PG MVP) — sem divergência real
**Para UI**: indiferente — cookie httpOnly é o contrato.

---

## 8. Pontos que NÃO mudaram da Turn 1 (reconfirmação)
- **ITCSS + BEM com namespacing** (Turn 1 §4) — ninguém tocou; mantém.
- **Tokens em 3 camadas** com dark mode via `[data-theme]` — mantém.
- **WCAG 2.1 AA** — mantém.
- **Vendor-commit HTMX/Alpine/Chart.js** — mantém. DevOps não propôs Node toolchain.
- **Endpoints JSON só para gráficos** — reforçado pela separação proxy/UI do Software Architect.
- **Telas Meus Tokens, Meu Uso, Provedores, Usuários** — mantêm, com adições de §2.3 (Auditoria) e §2.4 (KPI cached_tokens).

---

## 9. Perguntas diretas a pares (para consensus)

### Para Backend Architect
1. **Confirma server-rendered Beego para toda UI admin** (sem API REST admin completa, só controllers + partials HTMX + JSON isolado para gráficos)? Isto reduz seu trabalho — sem DTOs/OpenAPI para admin.
2. **`cached_tokens` no rollup**: DBA modelou no `usage_logs` mas não nos rollups. Posso contar com isso sendo agregado ao rollup horário/diário pelo job?

### Para DBA
3. **Adicionar `cached_tokens BIGINT` ao `usage_rollups_hourly` e `usage_rollups_daily`**? (Reflete §2.4 e §3.3 deste delta.)
4. **Endpoint `/api/v1/web/dashboard/usage` consulta rollups, nunca `usage_logs`** — confirma intenção do seu §3.4?

### Para Security Architect
5. **Formato `sk-aipp-[keyid8]-[secret48]`** — confirma que UI exibe máscara como `sk-aipp-AB12CD34-••••••••` (key_id visível, secret mascarado)?
6. **Tela de Auditoria** (minha §2.3) — concorda que admin precisa de UI para consultar `audit_log`? Campo adicional a exibir (ex.: `before`/`after` JSONB diff)?

### Para DevOps Engineer
7. **`readOnlyRootFilesystem: true` + `embed.FS`** — confirma que assets embedded no binário Go são compatíveis com esta constraint? (Minha leitura: sim, embed não escreve no disco.)
8. **Ingress path routing** (`/v1/*` proxy, `/admin/*` + `/me/*` UI) — confirma que um único Ingress com path-based routing serve ambos os estilos de handler?

### Para Software Architect
9. **Proxy handler puro + Controllers Beego no mesmo binário** — confirma que não há gotcha de inicialização (ex.: Beego captura todas as rotas e preciso registrar `/v1/*` antes)? Qual a ordem de registro recomendada?

---

## 10. Self-check (reflexão)
1. **Separei structure de skin?** Sim — Objects para layout, Components para UI específica. Novos (`c-quota-bar` futuro, `c-org-badge`) seguem o mesmo princípio.
2. **Evitei abstração prematura?** `c-org-badge` é um slot, não dropdown complexo — só vira dropdown quando multi-tenant chegar. Fluxo de login multi-step desenhado mas só step 1 ativo. Não abstrai antes do 3º uso.
3. **BEM names semânticos?** `c-key-row--auto-disabled` (não `--yellow`), `c-stat-card--featured` (não `--big-blue`). Sim.
4. **Especificidade estritamente crescente?** Mantida — Objects < Components < Utilities.
5. **Todos valores tokenizados?** Sim — estados `--success`/`--warning`/`--danger` já definidos em Turn 1, prontos para status badges e quota bars.
6. **Doc autossuficiente?** O delta deixa claro: formato do token, telas novas (Auditoria), KPI novo (cached_tokens), preparação multi-tenant (slot topbar + middleware + rotas).