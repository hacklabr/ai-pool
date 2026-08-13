# ADR-0011 — Frontend server-rendered (html/template + HTMX + ECharts), não SPA

**Status:** aceito

**Contexto:** O painel admin atende ~30 usuários internos. Uma SPA exigiria build pipeline de JS, API JSON completa e maior superfície de ataque (XSS, supply-chain de npm). O objetivo é binário único auto-contido.

**Decisão:** UI server-rendered com `html/template` (Go), interatividade via HTMX + Alpine.js, gráficos via ECharts. Templates e CSS próprio são embarcados no binário via `embed.FS`; as 3 bibliotecas JS vêm de CDN **com SRI** (`integrity=sha384-...`), com fallback de vendor-commit documentado. CSS é self-contained (sem framework CSS externo).

**Consequências:** Positivas: binário único; sem build de frontend; CSRF double-submit integra naturalmente com HTMX (header `X-CSRF-Token`). Negativas: dependência de CDN em runtime (mitigada por SRI; fallback documentado); interatividade limitada comparada a SPA (aceitável para o público); testes de UI são server-side apenas.

**Evidências:** `web/embed.go`; `web/templates/` (`layouts/base.html`, `partials/head.html`); `web/static/css/`; `internal/adapters/httpserver/admin/templates.go`.

**Origem:** DECISIONS.md A11 (consenso de especialistas 6-0).
