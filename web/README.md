# Diretório de assets da UI (server-rendered) — F8 entregue

UI admin do **AI Provider Pool** com 8 telas server-rendered (Go html/template
+ HTMX + Alpine.js + ECharts).

## Stack (spec §3.6, Frontend Architect)

| Camada | Tecnologia | Servida via |
|--------|-----------|-------------|
| Templates | Go `html/template` (stdlib) | **embed.FS** no binário único |
| Interação leve | HTMX 2.x | **CDN** (SRI) |
| Estados UI | Alpine.js 3.x | **CDN** (SRI) |
| Gráficos dashboard | Apache ECharts 5.x | **CDN** (SRI) |
| CSS | Vanilla + ITCSS + BEM | **embed.FS** |

### Decisão: embed vs CDN

- **CSS próprio** → embed.FS (essencial, pequeno ~30KB, self-contained).
- **JS libs (HTMX/Alpine/ECharts)** → CDN com SRI hashes (pragmático: ~30
  usuários internos com internet; ~600KB economizados no binário; SRI protege
  contra CDN comprometido).

Se rede interna exigir offline: baixar os arquivos para `web/static/vendor/`
e ajustar os `<script src>` em `web/templates/partials/head.html`. As tags
`integrity` podem ser removidas nesse caso (servindo do mesmo origin).

## Estrutura

```
web/
├── embed.go                    # //go:embed all:templates all:static
├── templates/
│   ├── layouts/base.html       # shell com topbar/sidebar + {{block "page-content"}}
│   ├── partials/               # head, topbar, sidebar (reusáveis)
│   └── pages/                  # 8 telas (cada uma define "page-content")
│       ├── login.html          # multi-step (senha + slot MFA + slot OAuth)
│       ├── dashboard.html      # admin: KPIs + 3 gráficos ECharts
│       ├── users.html          # admin: CRUD usuários
│       ├── providers.html      # admin: CRUD provedores
│       ├── keys.html           # admin: 4 estados visuais + force-enable/disable
│       ├── my_tokens.html      # qualquer role: gerar/revogar sk-aipp-...
│       ├── my_usage.html       # qualquer role: visão pessoal
│       └── audit.html          # admin: log append-only com filtros
├── static/
│   └── css/                    # design system ITCSS
│       ├── 01-settings.css     # tokens (cores, spacing, dark mode [data-theme])
│       ├── 02-tools.css        # (reservado para mixins)
│       ├── 03-generic.css      # reset, box-sizing, x-cloak, reduced-motion
│       ├── 04-elements.css     # bare HTML (body, h1-h6, forms, tables)
│       ├── 05-objects.css      # o-layout, o-stack, o-cluster, o-grid
│       ├── 06-components.css   # c-button, c-card, c-stat-card, c-status-badge,
│       │                        # c-data-table, c-modal, c-key-row, c-topbar,
│       │                        # c-sidebar, c-toast, c-login-form, etc.
│       ├── 07-utilities.css    # u-hidden, u-text-*, u-mt-*, etc.
│       └── main.css            # @import em ordem ITCSS
└── README.md (este arquivo)
```

## Design System — Tokens em 3 camadas

1. **Global (raw)**: paleta, escalas de espaçamento/tipografia (`--color-blue-500`, `--spacing-4`, etc.).
2. **Alias (semântico)**: nomes com propósito (`--color-primary`, `--color-danger`, `--color-surface`).
3. **Component (scoped)**: overrides locais em cada bloco (`--button-bg`, `--card-padding`).

**Dark mode**: `[data-theme="dark"]` no `<html>` sobrepõe apenas aliases. Toggle
via botão no topbar (Alpine persiste em `localStorage`).

## Acessibilidade (WCAG 2.1 AA)

- Contraste validado (4.5:1 texto, 3:0 UI).
- `:focus-visible` com outline token-based (nunca `outline: none` sem alternativa).
- Navegação por teclado (tab/enter/esc) em todos os modais/dropdowns.
- ARIA roles (`dialog`, `status`, `alert`, `nav`, `main`, `menuitem`).
- `aria-live="polite"` em toasts.
- `prefers-reduced-motion` respeitado.

## Segurança

- **CSRF**: cada form tem `<meta name="csrf-token">` + HTMX injeta `X-CSRF-Token`
  automaticamente (configurado em `head.html`). Mutations exigem header matching
  cookie (double-submit).
- **html/template escapa output por padrão** (defesa XSS automática).
- **Tokens nunca exibidos**: `sk-aipp-AB12CD34-••••••••` (key_id visível, secret
  mascarado). Token completo UMA ÚNICA VEZ na criação.
- **Chaves de provedor**: valor JAMAIS trafega pela UI — só label/status/metadata.
