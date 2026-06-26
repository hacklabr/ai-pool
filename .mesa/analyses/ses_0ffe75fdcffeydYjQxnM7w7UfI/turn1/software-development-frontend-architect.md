# Análise — Frontend Architect (Turno 1)

**Projeto:** AI Provider Pool — OpenAI-Compatible Proxy
**Foco:** Interface web de gestão + dashboard
**Stack backend:** Go + Beego + PostgreSQL · Deploy: K8s + Helm (prod), Docker Compose (dev)
**Público:** ~30 usuários internos · Escopo: painel admin + dashboard
**Veredito preliminar:** Server-rendered (templates Beego) + HTMX + Alpine.js + Chart.js. **NÃO recomendo SPA** para este projeto.

---

## 1. Decisão arquitetural: Server-rendered (HTMX) vs SPA

### Por que NÃO SPA (React/Vue/Svelte)

Para 30 usuários internos num painel admin CRUD-heavy, SPA introduz complexidade que não se paga:

| Custo de uma SPA | Impacto real neste projeto |
|---|---|
| Toolchain JS separada (Vite/esbuild, node_modules, lockfile) | Mais um repositório/caminho de build, CI bifurcado |
| Pipeline de deploy bifurcado (binário Go + assets estáticos) | K8s passa a ter 2 deployments ou binário com `embed.FS` |
| Autenticação JWT em SPA: token em localStorage é vulnerabilidade XSS; em memória perde no refresh; cookie httpOnly precisa de API REST com CORS | Superfície de ataque desnecessária |
| Estado de cliente (rotas, cache, loading, error) | Boilerplate massivo (TanStack Query, Pinia, Redux) para CRUDs que o servidor já conhece |
| Time precisa dominar React E Go | Curva e manutenção duplicadas |
| Bundle JS ~150–400 KB para 30 pessoas | Overhead sem retorno |

**SPA só se justificaria** se: (a) houvesse edição colaborativa em tempo real, (b) gráficos altamente interativos com drill-down aninhado, (c) público externo massivo, (d) time fortemente JS. Nenhuma condição se aplica aqui.

### Por que Server-rendered com HTMX é o ajuste ideal

- **Beego já renderiza templates** (`beego.Controller.TplName`) — zero nova infraestrutura.
- **Binário Go único** para K8s: templates embedded via `embed.FS` e servidos pelo próprio Beego. Helm chart permanece simples (1 Deployment, 1 Service, 1 Ingress).
- **HTMX** resolve 95% das interações (CRUD via `hx-post`, busca live, toggle de chave, paginação, modais) com HTML sobre o wire — sem JSON/serialização dupla.
- **State of the UI vive no servidor**, onde o PostgreSQL já conhece a verdade. Sem cache stale, sem optimistic UI frágil.
- **Segurança**: sessão em cookie `httpOnly` + `SameSite=Strict`, CSRF token do próprio Beego. Nada de tokens em localStorage.
- **Acessibilidade** mais fácil: HTML semântico é renderizado diretamente, sem hidratação que quebra ARIA.
- **Performance** para 30 pessoas: renderização server-side é trivial; HTMX troca apenas fragmentos (~2–10 KB por interação).

### Stack proposta (recomendada)

| Camada | Tecnologia | Por quê |
|---|---|---|
| Templates | Beego `html/template` (Go stdlib) | Já vem no framework; escaping automático |
| Interatividade leve | HTMX 2.x | CRUD, busca, modais, toggle — tudo via atributos HTML |
| Pequenos estados de UI | Alpine.js 3.x (~15 KB) | Dropdowns, tabs, confirmações, visibility de campos |
| Dashboard / gráficos | Apache ECharts ou Chart.js (ver §3) | Renderização client-side a partir de JSON servido pelo Beego |
| CSS | Vanilla CSS + ITCSS + BEM (ver §4) | Sem build CSS, sem framework |
| Ícones | Heroicons via SVG inline ou sprite | Sem dependência runtime |
| JS total | ~30 KB gzipped (HTMX + Alpine + Chart.js lazy-load) | Carregado só onde necessário |

**Princípio:** *HTML sobre o wire, JSON só onde a renderização é intrinsecamente client-side* (gráficos). Todo o resto é fragmento HTML servido pelo Beego.

### Integração com o backend Beego

- **Templates** em `views/` (embed no binário): `views/layouts/base.tpl`, `views/pages/*.tpl`, `views/partials/*.tpl`.
- **Partial rendering para HTMX**: Controller detecta header `HX-Request: true` e renderiza só o partial relevante, pulando o layout.
- **Endpoints JSON** (só para gráficos): `GET /api/v1/web/dashboard/usage` retorna JSON para Chart.js. **Endpoints distintos dos do proxy** (`/api/v1/web/*` vs `/v1/chat/completions`), autenticados por **cookie de sessão** (não pelo Bearer `sk-...`).
- **CSRF**: Beego middleware injeta token; HTMX envia automaticamente via header `X-CSRFToken` (configurado em `hx-headers` no `<body>`).

---

## 2. Áreas da interface — Mapeamento de telas/fluxos

### 2.1. Layout shell (comum a todas as telas)

Topbar (logo, toggle theme, user menu, sair) + Sidebar (nav). Itens de sidebar renderizados condicionalmente por role: itens admin só aparecem para `admin`. **Renderização no servidor = zero chance de vazamento por JS client-side.** Tema claro/escuro via `data-theme="dark"` no `<html>` + CSS custom properties.

### 2.2. Tela: Login
Form simples: username + password. POST via HTMX para `/auth/login`; sucesso → redirect via header `HX-Redirect: /dashboard`. Erro → partial injetado inline. Bloqueio após N tentativas. Sem OAuth no MVP, mas campo de design reservado para botão "Entrar com [Provider]" no futuro.

### 2.3. Tela: Dashboard (admin)
- **KPI cards**: total de requisições (período), tokens consumidos, taxa de erro, chaves ativas/desabilitadas. Cada card é `c-stat-card`.
- **Gráfico principal**: utilização por usuário (bar/line) — período selecionável (24h / 7d / 30d).
- **Gráficos secundários**: tokens por provedor (donut), taxa de erro por provedor (bar), top modelos (lista).
- **Tabela recente**: últimas N requisições (filtro live via HTMX).
- Dados via endpoint JSON `GET /api/v1/web/dashboard/usage` → Chart.js.

### 2.4. Tela: Gestão de usuários (admin)
Tabela: username, role, status, tokens ativos, uso total. Ações por linha (HTMX): editar, desabilitar/habilitar, excluir (confirmação Alpine), resetar senha. Modal de criação/edição via HTMX. Filtro e paginação server-side.

### 2.5. Tela: Gestão de provedores (admin)
Cards de provedor (`c-provider-card`): nome, base URL, User-Agent override (input editável), chaves vinculadas (count + status), modelos. CRUD inline com modais HTMX. Toggle ativo/inativo.

### 2.6. Tela: Gestão de chaves (admin) — a mais complexa
Múltiplos estados por chave:
- **Status visual** (badge `c-status-badge` com modifiers `--active`, `--disabled`, `--auto-disabled`, `--scheduled-reactivation`): Ativa, Desabilitada manualmente, Auto-desabilitada (com **data de reativação agendada** mostrada — crítica para UX), Em retry/backoff.
- **Ações**: forçar habilitar/desabilitar (toggle HTMX), ver histórico de erros, testar chave (ping manual), editar User-Agent override.
- **Listagem agrupada por provedor** (accordion Alpine ou seções HTMX).
- Indicador visual claro quando uma chave está em "reativação agendada" com contagem regressiva.

### 2.7. Tela: Meus tokens (usuário comum E admin para si)
Lista de tokens do usuário logado: prefixo (`sk-abc...`), data criação, último uso, status. **Geração**: botão "Gerar novo token" → modal → form → **token completo mostrado UMA ÚNICA VEZ** (banner de aviso) com botão copiar. **Revogação**: HTMX com confirmação dupla.

### 2.8. Tela: Meu uso (usuário comum)
Visão pessoal: tokens consumidos no período, requisições por dia, breakdown por modelo/provedor, custos (roadmap). Gráfico Chart.js com dados do próprio usuário. **Reutiliza componentes do Dashboard admin**, mas com escopo filtrado server-side pelo `user_id` da sessão.

### 2.9. Diferenciação admin vs usuário comum
- **Renderização condicional no servidor**: rotas admin (`/admin/*`) protegidas por middleware Beego que valida role. Itens de menu admin só emitidos no HTML se o usuário for admin.
- **Defesa em profundidade**: verificação de role NÃO vive no frontend. Mesmo que usuário comum tente acessar `/admin/users`, middleware retorna 403 antes de renderizar. A UI nunca é a única barreira.
- **Rota raiz**: `/` redireciona para `/dashboard` se admin, para `/me/usage` se usuário comum.

---

## 3. Dashboard de utilização — Gráficos e dados

### Biblioteca de charting
**Recomendação: Apache ECharts** para o gráfico principal (mais robusto para grandes volumes, suporte nativo a zoom/brush, decência com dark mode). **Chart.js** é alternativa mais leve. Lazy-load do bundle de charts apenas nas páginas `/dashboard` e `/me/usage`.

### Métricas recomendadas

| Gráfico | Tipo | Eixo X | Eixo Y / Série |
|---|---|---|---|
| Utilização por usuário (admin) | Bar horizontal | Usuário | Tokens consumidos (ou reqs) |
| Consumo temporal | Line/area | Tempo (agregado) | Tokens por período |
| Distribuição por provedor | Donut | — | % de tokens por provedor |
| Taxa de erro por chave/provedor | Stacked bar | Provedor | sucessos vs erros |
| Top modelos | Lista ranqueada (HTML) | — | Modelo + contagem |

### Endpoints de agregação
```
GET /api/v1/web/dashboard/usage?from=&to=&granularity=hour|day&metric=tokens|requests&group_by=user|provider|model
→ 200 { "series": [{"label":"joao","data":[...]}], "buckets": [...] }
```
- **Granularidade adaptativa**: por hora para janelas ≤ 7d, por dia para maiores.
- **Cache curto (60s)** no handler — beego cache ou Redis. Para 30 usuários, até query direta com índice adequado aguenta.
- **Paginação de buckets**: nunca retornar > 365 pontos.

### Performance com volume
- ~30 usuários × baixa QPS: sem otimização heroica no MVP.
- **Índice crítico**: `(user_id, created_at DESC)` e `(provider_id, created_at DESC)` na tabela de logs.
- Agregações via SQL `GROUP BY` + `date_trunc()` — não processar em Go.
- Roadmap (custos + escala): materialized views refreshadas por job, ou mover para Prometheus + Grafana (observabilidade, não produto).

---

## 4. Design system / Componentização

### Arquitetura CSS: ITCSS + BEM com namespacing

Para um painel admin que crescerá com o roadmap (times, quotas, OAuth config), disciplina ITCSS desde o dia 1 evita a dívida técnica clássica de "CSS admin bagunçado".

#### Estrutura de arquivos
```
web/static/css/
├── 01-settings/          # Tokens (CSS custom properties)
│   ├── tokens-color.css
│   ├── tokens-spacing.css
│   ├── tokens-typography.css
│   ├── tokens-shadow.css
│   └── tokens-motion.css
├── 02-tools/             # Mixins / functions
├── 03-generic/           # Reset, box-sizing, normalize
├── 04-elements/          # Bare HTML elements
├── 05-objects/           # Estrutura/layout (structure-only)
│   ├── o-layout.css, o-stack.css, o-cluster.css, o-grid.css, o-sidebar.css
├── 06-components/        # UI específico (skin + comportamento)
│   ├── c-button, c-card, c-stat-card, c-status-badge, c-data-table,
│   ├── c-form-field, c-modal, c-key-row, c-provider-card, c-dropdown,
│   ├── c-toast, c-tabs, c-chart-card, c-login-form
├── 07-utilities/         # Overrides single-rule
│   ├── u-spacing, u-text, u-display, u-color
└── main.css              # @import em ordem
```

#### Naming Convention
- **BEM estrito** para componentes: `.c-key-row__status--auto-disabled`.
- **Namespacing obrigatório**: `o-`, `c-`, `u-`, `is-`/`has-` para estados (`.c-key-row.is-loading`).
- **Sem seletores aninhados profundos**. Especificidade cresce estritamente de Settings → Utilities. Um `u-hidden` sempre vence de um `c-card`.
- **Sem IDs como seletores**. Sem `!important` (reservado a utilitários com justificativa).

#### Tokens de Design — Hierarquia em 3 camadas

**Global (raw)** — valores brutos:
```css
:root {
  --color-blue-500: #3B82F6;
  --color-red-500: #EF4444;
  --spacing-1: 0.25rem;
  --spacing-4: 1rem;
  --radius-md: 0.375rem;
}
```

**Alias (semântico)** — nomes com propósito:
```css
:root {
  --color-primary: var(--color-blue-500);
  --color-danger: var(--color-red-500);
  --color-text: var(--color-gray-900);
  --color-surface: #FFFFFF;
}
[data-theme="dark"] {
  --color-text: #F9FAFB;
  --color-surface: #1F2937;
}
```

**Component (scoped)** — overrides locais:
```css
.c-stat-card {
  --stat-card-padding: var(--space-component-gap);
  --stat-card-value-color: var(--color-text);
  padding: var(--stat-card-padding);
}
.c-stat-card--danger { --stat-card-value-color: var(--color-danger); }
```

**Regra absoluta**: nenhum valor hardcoded em component files. Tudo é token.

### Componentes críticos do MVP (exemplo de decomposição)
```
## Component: c-key-row
ITCSS Layer: Component
BEM Block: .c-key-row
Elements:
  - __label, __provider, __status, __actions, __meta
Modifiers:
  - --auto-disabled, --scheduled-reactivation
States:
  - .is-loading, .is-disabled
Dependencies: c-status-badge, c-button
Tokens: --key-row-bg, --key-row-border, --key-row-padding
```

### Acessibilidade (WCAG 2.1 AA, mínimo)
- **Contraste**: tokens validados contra AA (4.5:1 texto, 3:1 UI). Ambos os temas.
- **Foco visível**: `:focus-visible` com outline token-based, nunca `outline: none` sem alternativa.
- **Teclado**: toda ação (toggle de chave, modal, tabs) navegável por tab/enter/esc.
- **ARIA**: roles para `dialog`, `status`, `alert`, `nav`, `main`. `aria-live="polite"` para atualizações HTMX dinâmicas.
- **Formulários**: `<label>` explícito, `aria-describedby` para erros, validação server-side.
- **Tabelas**: `<th scope>`, caption, sem layout table para visual.
- **Responsividade**: design mobile-first; sidebar vira drawer < 768px.

### Responsividade
- Breakpoints: `640px`, `768px`, `1024px`, `1280px`.
- Layout principal: sidebar fixa ≥1024px, drawer <1024px.
- Cards de dashboard: grid 1 col mobile → 2 col tablet → 3-4 col desktop.

---

## 5. Autenticação no frontend

### Decisão: sessão server-side via cookie httpOnly
- **NÃO usar JWT para a UI web**. JWT em SPA/localStorage é dor; em cookie httpOnly sem refresh é estado inconsistente.
- **Sessão tradicional**: Beego session store (memória em dev, Redis em prod) com session ID aleatório de alta entropia em cookie `httpOnly; Secure; SameSite=Strict; Path=/`.
- **CSRF**: token Beego injetado em `<meta name="csrf-token">`, lido por HTMX e enviado em toda mutation. Somente rotas não-GET exigem CSRF.
- **Expiração**: sessão sliding de 8h (renovada a cada req), hard expiry 24h. Logout invalida server-side.
- **Proteção de rotas admin**: middleware Beego `web.Filter` valida role antes de renderizar — UI nunca é a barreira.

### Separação crítica de autenticação

| Superfície | Auth | Header |
|---|---|---|
| API do proxy (`/v1/chat/completions`) | Token `sk-...` do usuário | `Authorization: Bearer sk-...` |
| UI web + endpoints `/api/v1/web/*` | Sessão (cookie httpOnly) | Cookie de sessão + CSRF |

**Por que separar**: o token `sk-...` é usado em clientes de IA. Ele NÃO deve autenticar a UI administrativa — seria vetor de privilege escalation se vazasse. A UI usa sessão própria.

### Refresh / expiração
- Sessão expira → 401 → interceptor HTMX (`htmx:responseError`) redireciona para `/login` com `next` param.
- Sem refresh tokens, sem fluxo complexo. Simplicidade vence.

---

## 6. Estado e data fetching

### Princípio: estado vive no servidor
HTMX elimina a maior parte do "estado da aplicação":
- **Listas/tabellas**: HTML re-renderizado após mutation, sem cache client-side para invalidar.
- **Form errors**: partial HTML com erros retorna no response, injetado no form.
- **Loading state**: `hx-indicator` mostra spinner automaticamente.
- **Toast/notificação**: `HX-Trigger` header emite evento → Alpine escuta e mostra `c-toast`.

### Estado client-side (mínimo, com Alpine.js)
Apenas estados efêmeros de UI: dropdown aberto/fechado, tab ativa, modal visível, confirmação de deleção, toggle de tema, filtros visuais antes de submeter. **Nada de store global (Pinia/Redux/Zustand).**

### Data fetching
- **HTMX**: `hx-get`, `hx-post` com `hx-target`, `hx-swap`. Padrão de ouro.
- **Chart.js**: único consumidor de JSON. `fetch()` nativo no `init`. Sem biblioteca de data fetching.
- **Polling opcional** (dashboard ao vivo): `hx-trigger="every 30s"`. Cuidado com custo — opcional, desligável.

### Loading / error / empty states
Padronizar três partials reutilizáveis: `partials/ui/loading.tpl`, `partials/ui/error.tpl`, `partials/ui/empty.tpl`. Cada listagem/tabela tem os três estados. Nenhum estado "vazio invisível" — sempre feedback.

---

## 7. Build e deploy do frontend

### Estratégia: embedded assets no binário Go
```
ai-pool/
├── web/
│   ├── static/        # CSS, JS (HTMX, Alpine, Chart.js), imagens
│   ├── views/         # templates Beego (.tpl)
│   └── embed.go       # //go:embed static/* views/*
├── cmd/server/main.go
```
- `//go:embed` empacota tudo dentro do binário Go. K8s deployment = 1 container, 1 binário.
- Beego configurado para servir do `embed.FS` em prod.
- Em dev, servir do disco para hot-reload de templates/CSS (Beego respeita `RunMode == dev`).

### Pipeline de assets
- **CSS**: vanilla, sem Sass/PostCSS obrigatório no MVP. Se quiser future-proofing, **Lightning CSS** (single binary, sem Node). Opcional.
- **JS**: HTMX, Alpine e Chart.js como arquivos vendor em `web/static/vendor/` (commitados) ou via npm + cópia. **Recomendo vendor-commit** para não introduzir Node toolchain.
- **Build**: `go build` único. Makefile target `make build` → binário com tudo embutido.
- **Sem webpack, sem Vite, sem esbuild**. Overhead para este escopo.

### Deploy em produção (K8s + Helm)
- Container: `FROM gcr.io/distroless/static` (binário Go estático, sem shell, sem CVEs).
- 1 Deployment, 1 Service (ClusterIP), 1 Ingress com TLS.
- Liveness/readiness probes em endpoints Beego `/healthz`.
- Assets servidos pelo próprio Beego — sem nginx, sem CDN (30 usuários internos).
- Config via ConfigMap/Secret (DSN, secret de sessão).

---

## 8. Extensibilidade para o ROADMAP

A arquitetura precisa acomodar crescimento sem rewrite. Mapeio cada item do roadmap para impacto na UI:

| Item do roadmap | Impacto na UI | Como preparar agora |
|---|---|---|
| **Times** | Nova entidade → sidebar item "Times", seletor de contexto no topbar | `o-layout-sidebar` com slots para novos nav items; topbar com espaço para seletor |
| **Quotas por usuário/time** | Novo painel de quotas, progress bars, alertas | Componente `c-quota-bar` futuro; tokens de "warning/danger" já prontos |
| **Limitação de horário** | Form de config por usuário | Reutilizar `c-form-field` |
| **Multi-tenancy** | Seletor de organização no topbar, telas com escopo | Rotas com prefixo `/org/:org_id/*` desde o início; middleware de scoped queries |
| **OAuth** | Botão "Entrar com X" no login | Login form deixa slot para métodos alternativos; isolamento da estratégia de auth no backend |
| **Prometheus /metrics** | Sem impacto na UI (observabilidade, não produto) | Nada |
| **Model aliasing** | UI de mapeamento alias → modelo | CRUD genérico reutilizável |
| **Health check de chaves** | Indicador visual de "último probe" na key row | `c-key-row` com slot `__meta` já existente |
| **Notificação/webhook** | Form de config por chave | Componente de config genérico |
| **Custo por token** | Coluna de custo em tabelas, KPI de custo | Tabelas com colunas opcionais; tokens de moeda |
| **Failover cross-provider** | Visualização de cadeia de failover | Tabela de tentativas (partial reutilizável) |
| **Modelo "auto"** | Config de regras de roteamento | UI de regras (genérica) |

### Princípios de design extensível
1. **Componentes BEM isolados** — adicionar feature não exige editar componente existente.
2. **Sidebar data-driven** — itens de nav são uma lista (slice Go), não hardcoded no template.
3. **Rotas com namespace futuro** — estruturar como `/app/*` deixando espaço para `/org/:id/*` ou `/:tenant/*`.
4. **Tokens de cor neutros para estados** — `--color-success`, `--color-warning`, `--color-danger` já definidos.
5. **Layout shell flexível** — topbar/sidebar são partials compostos; novos elementos entram como slots.
6. **Sem acoplamento UI ↔ regra de negócio** — UI sempre lê estado do backend.

---

## 9. Riscos e trade-offs honestos

| Risco | Mitigação |
|---|---|
| HTMX exige mentalidade server-first que time JS pode não dominar | Documentar padrões; HTMX é simples (atributos HTML). Curva < 1 dia. |
| Chart.js/ECharts é a única peça "JS pesada" — se quebrar, dashboard quebra | Lazy-load isolado; fallback HTML table para dados críticos |
| Sem SPA = transições menos "fluidas" | Para painel admin, é aceitável. HTMX com animações CSS cobre 90% |
| Alpine + HTMX podem parecer sobreposição | São complementares: HTMX = servidor → cliente, Alpine = estado client puro |
| CSS vanilla sem framework pode parecer "lento" no início | ITCSS + tokens dão velocidade após setup inicial (1 sprint) |

---

## 10. Recomendação final consolidada

1. **Server-rendered com Beego templates + HTMX + Alpine.js + Chart.js**. Não SPA.
2. **Binário Go único** com assets `embed.FS`. Deploy K8s simples.
3. **Sessão httpOnly + CSRF**, separada da auth `sk-...` do proxy.
4. **ITCSS + BEM com namespacing**, tokens em 3 camadas (global/alias/component), dark mode nativo via `[data-theme]`.
5. **Endpoints JSON só para gráficos** em `/api/v1/web/*`; todo resto é HTML sobre o wire.
6. **Componentes BEM documentados**, design system preparado para crescimento (times, quotas, multi-tenant).
7. **Acessibilidade WCAG AA** desde o MVP, não como retrofit.
8. **Sem Node toolchain obrigatório** — vendor-commit de HTMX/Alpine/Chart.js. CSS vanilla com Lightning CSS opcional.

Esta arquitetura entrega o escopo do MVP em **menos código, menos risco, menos deploy, menos ataque** do que uma SPA — e está pronta para absorver o roadmap sem rewrite.