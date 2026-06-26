# Analysis: Frontend Architect — Turn 3

**Agent ID:** software-development-frontend-architect
**Timestamp:** 2026-06-25T21:35:30.872Z

# Posição Final de Consenso — Frontend Architect (Turno 3)

## 1. Voto: Opção A (1 réplica, sem Redis no MVP) — CONCORDO PLENO

Consenso unânime 5-0. Como frontend, **1 réplica simplifica tudo**:
- **Sessão in-memory suficiente** — sem Redis, sem sticky sessions, sem invalidação pub/sub.
- **Cookie httpOnly + SameSite=Strict** funciona sem coordenação entre pods.
- **Estados HTMX (loading, toasts, transições) consistentes** — sem race entre pods re-renderizando a mesma linha.
- **Graciosidade de deploy**: 1 réplica + `strategy: Recreate` + graceful shutdown (confirmado MVP) = downtime de segundos, aceitável para 30 usuários internos.

**Roadmap multi-réplica**: UI não muda. O contrato (cookie httpOnly) é estável; só o backend troca a store de sessão (memória → Redis). **Zero rework frontend** — teste da arquitetura.

---

## 2. Resolução das 9 perguntas T2 — TODAS FECHADAS

| # | Pergunta T2 | Resolução |
|---|---|---|
| 1 | Backend confirma server-rendered sem API REST admin completa | ✅ **Backend (T3)**: "Telas admin consomem HTML sobre o wire via HTMX. Apenas JSON isolado para gráficos do dashboard. Nada de CRUD REST genérico." |
| 2 | `cached_tokens` agregado no rollup | ✅ **Backend (T3)**: "Job (goroutine `time.Ticker(1h)`) soma `cached_tokens` no rollup via upsert idempotente. Retentativa em falha." |
| 3 | (Duplicada com #2 — DBA) | ✅ **DBA (T3)**: rollups como tabelas com `cached_tokens`. |
| 4 | Endpoint consulta rollups | ✅ **Backend (T3)** com ressalva aceita: "Qualquer `GROUP BY` ou série temporal → rollup; lista paginada recente (últimas 50) → `usage_logs` da partição corrente com prune (sub-ms)." **Adoto**: tabela "recentes" do dashboard consulta logs crus da partição atual; demais agregações via rollup. |
| 5 | Formato `sk-aipp-[keyid8]-[secret48]` | ✅ **Adoto Security (§4.1)**. Máscara UI: `sk-aipp-AB12CD34-••••••••`. Token completo UMA ÚNICA VEZ na criação. Prefixo registrável no GitHub secret scanning. |
| 6 | Tela de Auditoria | ✅ **Adicionada ao MVP** (Admin). Read-only, consulta `audit_log` (Security §10.1 / DBA §2.11). Filtros por ator/ação/período via HTMX, modal com diff `before`/`after` JSONB. Reutiliza `c-data-table` + `c-modal`. |
| 7 | embed.FS + readOnlyRootFilesystem | ✅ **DevOps (T3)** confirmou. Assets embedded não escrevem no disco; `emptyDir` em `/tmp` cobre temporários. |
| 8 | Ingress path routing serve ambos handlers | ✅ **DevOps (T3)** confirmou. `/v1/*` (proxy, Bearer), `/admin/*` + `/me/*` + `/api/v1/web/*` (UI, sessão). |
| 9 | Proxy handler + Beego mesmo binário | ✅ **Software Architect (ADR-007 + T3)** confirmou. Sem gotcha; ordem de registro de rotas é detalhe backend. |

**Bonus do Backend (T3)** adotado como padrão frontend:
- Mutations admin (force-disable/enable key, revoke token) = `POST` HTMX retornando partial re-renderizado + `HX-Trigger` para toast. **Sem JSON roundtrip.**
- `422` → partial de erros inline. `409` (race via `version` col, locks otimistas DBA §7.2) → toast de conflito com botão "recarregar".

---

## 3. Decisões FRONTEND do MVP — FECHADAS (minha responsabilidade)

### 3.1. Stack frontend
- **Server-rendered Beego templates** (Go stdlib `html/template`) — NÃO SPA.
- **HTMX 2.x** para interatividade (CRUD, busca, modais, toggle, paginação).
- **Alpine.js 3.x** (~15 KB) para estados efêmeros de UI (dropdowns, tabs, confirmações, login multi-step).
- **Apache ECharts 4.x** para gráficos, lazy-load só em `/dashboard` e `/me/usage`. Chart.js como fallback aceitável.
- **Vendor-commit** de HTMX/Alpine/Chart.js em `web/static/vendor/` — sem Node toolchain obrigatório. CSS vanilla (Lightning CSS opcional).

### 3.2. Empacotamento e deploy
- **`//go:embed`** empacota templates + assets no binário Go único.
- K8s: 1 Deployment, 1 Service, 1 Ingress com path routing. Compatível com `readOnlyRootFilesystem: true` + `runAsNonRoot` + distroless/base-debian12.
- Dev: Beego `RunMode=dev` serve do disco (hot reload de `.tpl`/`.css`); `air` para reload de Go.

### 3.3. Arquitetura CSS
- **ITCSS em 7 camadas**: Settings → Tools → Generic → Elements → Objects → Components → Utilities.
- **BEM estrito** com namespacing: `o-` (Objects), `c-` (Components), `u-` (Utilities), `is-`/`has-` (States).
- **Tokens em 3 camadas**: Global (raw) → Alias (semântico) → Component (scoped). Regra: nenhum valor hardcoded em component files.
- **Dark mode** via `[data-theme="dark"]` no `<html>` + override de alias tokens. Preferência em cookie/localStorage.
- Especificidade estritamente crescente; `u-hidden` sempre vence `c-card`.

### 3.4. Autenticação e sessão
- **Sessão server-side via cookie httpOnly + Secure + SameSite=Strict + Path=/**. MaxAge 8h, sliding renewal, hard expiry 24h.
- **CSRF** via Beego middleware: token em `<meta name="csrf-token">`, HTMX envia automaticamente via `hx-headers` no `<body>`. Apenas mutations exigem.
- **Auth segregada por rota** (Security §6.2 + Backend T3): `/v1/*` = Bearer `sk-aipp-...`; `/admin/*` + `/me/*` + `/api/v1/web/*` = sessão cookie + CSRF. Nunca cruzam.
- **RBAC default-deny duas cadeias** (Security): middleware Beego valida role antes de renderizar admin. UI nunca é barreira — renderização condicional é conveniência, não segurança.
- **Expiração de sessão**: HTMX `htmx:responseError` handler (Alpine) captura 401/302 → toast warning + redirect `/login?next=<path>` preservando contexto.
- **DB hit por request para validação de token** (consenso unânime) — alinhado com revogação instantânea do briefing.

### 3.5. Áreas da UI (MVP)
1. **Login** — multi-step desde o MVP (step 1: senha ativo; step 2: MFA desenhado mas oculto; slot OAuth reservado).
2. **Dashboard (admin)** — KPI cards (reqs, tokens, **tokens economizados por cache — destacado** como diferencial, taxa de erro, chaves ativas/desabilitadas), gráfico principal utilização por usuário, gráficos secundários (donut provedores, bar erros, top modelos), tabela recentes (live tail via logs da partição corrente).
3. **Gestão de usuários (admin)** — CRUD, roles, reset senha, desabilitar.
4. **Gestão de provedores (admin)** — Cards com base_url, User-Agent override, modelos vinculados.
5. **Gestão de chaves (admin)** — A mais complexa. **4 estados visuais** via `c-status-badge`:
   - `--active` (verde): no pool, candidata LRU.
   - `--disabled-manual` (cinza): admin forçou; só admin reativa.
   - `--auto-disabled` (amarelo): sistema desabilitou; **mostra data de reativação agendada** (`auto_reactivate_at`).
   - `--in-backoff` (laranja): em retry com backoff crescente.
   - Ações: force enable/disable (HTMX), ver histórico de erros, testar chave, editar User-Agent override.
6. **Meus tokens** — Geração com token mostrado UMA VEZ; máscara permanente `sk-aipp-AB12CD34-••••••••`; revogação com confirmação dupla.
7. **Meu uso** — Visão pessoal filtrada server-side por `user_id` da sessão; reutiliza componentes do Dashboard admin.
8. **Auditoria (admin)** — **NOVA no MVP**. Read-only, `c-data-table` com filtros (ator, ação, entidade, período), paginação server-side, modal expandível com diff `before`/`after` JSONB.

### 3.6. Dashboard — contrato JSON
- `GET /api/v1/web/dashboard/usage?from=&to=&granularity=hour|day&metric=tokens|requests|errors|cached_tokens|latency&group_by=user|provider|model`
- **granularity=hour** → `usage_rollups_hourly` (janelas ≤30d). **granularity=day** → `usage_rollups_daily` (>30d).
- **Exceção (Backend T3)**: "últimas N requisições" (live tail) consulta `usage_logs` da partição corrente com prune — sub-ms.
- Response: `{ granularity, metric, group_by, series[], buckets[], totals{}, "partial"?: true }`.
- Cache handler: **300s (5min)** — frescura máxima do rollup é 1h.
- KPI destacado: **"Tokens economizados por cache de contexto"** — narrativa do diferencial do produto. `c-stat-card--featured`.

### 3.7. Acessibilidade
- **WCAG 2.1 AA** mínimo: contraste validado (4.5:1 texto), `:focus-visible` com outline token-based, navegabilidade teclado total (tab/enter/esc), ARIA roles (`dialog`, `status`, `alert`, `nav`, `main`), `aria-live="polite"` para toasts HTMX, `<label>` explícito + `aria-describedby` para erros de form, `<th scope>` em tabelas.
- Responsividade mobile-first: breakpoints 640/768/1024/1280px. Sidebar fixa ≥1024px, drawer <1024px. Cards: 1 col mobile → 2 col tablet → 3-4 col desktop.

### 3.8. Multi-tenant — preparação barata (MVP absorve roadmap)
- **Middleware Beego injeta `organization_id`** (DEFAULT 1) no contexto de toda requisição admin (DBA Opção A: `organization_id BIGINT NOT NULL DEFAULT 1`).
- **Topbar com slot** para seletor de organização (`c-org-badge`): MVP mostra "Default" estático; futuro vira `<select>` Alpine com `hx-get` que re-renderiza com `org_id` no contexto. **Custo: 1 partial com slot.**
- **Rotas com namespace futuro**: `/admin/*` implicitamente escopado por sessão; futuro `/org/:slug/admin/*` sem mudar controllers.
- **Quotas/time windows** (DBA cria tabelas vazias): componentes futuros (`c-quota-bar`, time picker) já previstos no design system — tokens `--color-warning`/`--color-danger` definidos. Items do roadmap = "popular dados", não "redesignar UI".

### 3.9. Login multi-step (preparar MFA/OAuth agora)
- **Step 1 ativo (MVP)**: username + password. `<slot name="alt-auth">` no rodapé — vazio no MVP, recebe "Entrar com Google" quando OAuth chegar.
- **Step 2 desenhado mas oculto**: código TOTP de 6 dígitos, ativado quando `user.mfa_enabled=true` (schema Security §5.4).
- **Custo agora**: ~1h de design de componente. **Economia futura**: redesign evitado.

---

## 4. Pontos de discórdia técnica — NENHUM

Consenso total. Não registro divergências. Todos os pontos maiores (server-rendered, embed.FS, sessão httpOnly, rollups com cached_tokens, formato sk-aipp-, Opção A 1 réplica, DB hit por request) foram endossados unânimes ou adotados pela posição majoritária.

---

## 5. Confirmação de FECHAMENTO

**O consenso está FECHADO da perspectiva do Frontend Architect.**

Todos os pontos críticos da arquitetura frontend estão decididos e endossados:
- ✅ Stack: Beego templates + HTMX + Alpine.js + ECharts (não SPA).
- ✅ Empacotamento: embed.FS no binário único, compatível com securityContext restrictivo.
- ✅ CSS: ITCSS + BEM namespacing + tokens 3 camadas + dark mode.
- ✅ Auth: sessão httpOnly+CSRF segregada do Bearer do proxy.
- ✅ UI: 8 áreas mapeadas (incluindo Auditoria nova no MVP).
- ✅ Dashboard: endpoint JSON consultando rollups, KPI de cached_tokens como diferencial.
- ✅ Token: formato `sk-aipp-AB12CD34-••••` (máscara UI).
- ✅ Multi-tenant: middleware + slot topbar + namespace rotas (preparação barata).
- ✅ MFA/OAuth: login multi-step desenhado agora.
- ✅ Acessibilidade WCAG AA, responsividade mobile-first.
- ✅ Opção A: 1 réplica simplifica UI (sem consistência de sessão entre pods).

**A arquitetura frontend está pronta para especificação.**