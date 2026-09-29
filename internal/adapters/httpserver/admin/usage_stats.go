package admin

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
)

// =============================================================================
// Painel de Estatísticas de Utilização (admin).
//
// GET  /web/admin/usage            — página server-rendered (form GET + KPIs).
// GET  /api/web/admin/usage/stats  — JSON (totals + série + breakdowns) p/ ECharts.
//
// Fonte: usage_logs DIRETO (não rollups) — os filtros por key/model não existem
// como dimensão de rollup, e a janela é limitada a 90d (retenção dos logs,
// spec §3.4.2). O handler faz clamp defensivo da janela.
//
// Filtros chegam por query string (form GET → URL bookmarkable):
//   from, to       — "2006-01-02" (input date) ou RFC3339; default 7d.
//   preset         — "24h"|"7d"|"30d" (sobrepõe from/to quando presente).
//   user_id        — api_tokens.user_id (0/ausente = todos).
//   key_id         — provider_keys.id (0/ausente = todas).
//   provider_id    — providers.id (0/ausente = todos).
//   model          — models.name exato (vazio = todos).
// Valores inválidos são IGNORADOS (caem no default) — mesmo comportamento
// tolerante do dashboard (parseDashboardWindow), nunca rejeitam a request.
// =============================================================================

// usageStatsMaxWindow limita a janela à retenção dos logs crus (90d).
const usageStatsMaxWindow = 90 * 24 * time.Hour

// usageStatsFilter é o estado de filtros parseado da query string.
// Fields com ponteiros nil = filtro ausente.
type usageStatsFilter struct {
	From       time.Time
	To         time.Time
	Preset     string // "24h"|"7d"|"30d"|"" (personalizado) — para marcar o select
	UserID     *int64
	KeyID      *int64
	ProviderID *int64
	Model      string
}

// parseUsageStatsFilter extrai e normaliza os filtros da query string.
// Janela: preset > from/to explícitos > default 7d. Clamp de 90d e garantia
// from < to (defesa contra params invertidos/manipulados).
func parseUsageStatsFilter(r *http.Request) usageStatsFilter {
	q := r.URL.Query()
	to := time.Now()
	from := to.AddDate(0, 0, -7)

	f := usageStatsFilter{Preset: q.Get("preset")}
	if f.Preset != "24h" && f.Preset != "7d" && f.Preset != "30d" {
		f.Preset = "" // personalizado (ou inválido) — datas mandam
	}
	if f.Preset != "" {
		switch f.Preset {
		case "24h":
			from = to.Add(-24 * time.Hour)
		case "30d":
			from = to.AddDate(0, 0, -30)
		default:
			from = to.AddDate(0, 0, -7)
		}
	} else {
		if v := q.Get("from"); v != "" {
			if t, err := parseUsageStatsTime(v); err == nil {
				from = t
			}
		}
		if v := q.Get("to"); v != "" {
			if t, err := parseUsageStatsTime(v); err == nil {
				// "to" inclusivo para o usuário: input date tem 00:00, então
				// estende até o fim do dia. RFC3339 com hora é respeitado.
				if len(v) == len("2006-01-02") {
					t = t.Add(24*time.Hour - time.Second)
				}
				to = t
			}
		}
	}

	// Garantias: from < to; janela ≤ 90d; to ≤ agora.
	if !from.Before(to) {
		from = to.AddDate(0, 0, -7)
	}
	if to.After(time.Now()) {
		to = time.Now()
	}
	if to.Sub(from) > usageStatsMaxWindow {
		from = to.Add(-usageStatsMaxWindow)
	}

	f.From, f.To = from, to
	f.UserID = usageStatsOptID(q.Get("user_id"))
	f.KeyID = usageStatsOptID(q.Get("key_id"))
	f.ProviderID = usageStatsOptID(q.Get("provider_id"))
	// Model: string livre, mas vai SEMPRE como placeholder ($n) — nunca vira
	// SQL. Cap defensivo de comprimento (nomes de modelo são curtos).
	if m := q.Get("model"); m != "" {
		if len(m) > 128 {
			m = m[:128]
		}
		f.Model = m
	}
	return f
}

// parseUsageStatsTime aceita "2006-01-02" (input date) e RFC3339 (API/JS).
func parseUsageStatsTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", v)
}

// usageStatsOptID converte "0"/""/inválido em nil (sem filtro); >0 em ponteiro.
func usageStatsOptID(v string) *int64 {
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

// toPostgresFilters converte o filtro de transporte para o do repositório.
func (f usageStatsFilter) toPostgresFilters(orgID int64) postgres.UsageStatsFilters {
	return postgres.UsageStatsFilters{
		OrgID:      orgID,
		From:       f.From,
		To:         f.To,
		UserID:     f.UserID,
		KeyID:      f.KeyID,
		ProviderID: f.ProviderID,
		Model:      f.Model,
	}
}

// usageStatsOption é um item de dropdown de filtro (id + rótulo).
type usageStatsOption struct {
	ID    int64
	Label string
}

// usageStatsOptions carrega usuários, chaves, provedores e modelos da org
// para popular os selects do formulário. Queries diretas simples (a página
// admin já consulta pools assim — ver handlePageAdminUsers). Tolerante a erro:
// dropdown vazio é melhor que página quebrada.
func (a *App) usageStatsOptions(ctx context.Context, orgID int64) (users, keys, providers, models []usageStatsOption) {
	load := func(q string, dst *[]usageStatsOption) {
		rows, err := a.pool.Query(ctx, q, orgID)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var opt usageStatsOption
			if err := rows.Scan(&opt.ID, &opt.Label); err == nil {
				*dst = append(*dst, opt)
			}
		}
	}
	load(`SELECT id, username FROM users WHERE organization_id = $1 ORDER BY username`, &users)
	load(`SELECT id, label FROM provider_keys WHERE organization_id = $1 ORDER BY label`, &keys)
	load(`SELECT id, slug FROM providers WHERE organization_id = $1 ORDER BY slug`, &providers)
	// Inclui inativos: filtrar por modelo removido do catálogo continua útil
	// (histórico de uso preservado nos logs).
	load(`SELECT id, COALESCE(display_name, name) FROM models WHERE organization_id = $1 ORDER BY COALESCE(display_name, name)`, &models)
	return
}

// usageStatsBucket escolhe o bucket da série: hora para janelas curtas
// (≤ 48h), dia para o resto — evita gráficos com 1 ponto (day) ou 2160 pontos
// (hour em 90d).
func usageStatsBucket(from, to time.Time) string {
	if to.Sub(from) <= 48*time.Hour {
		return "hour"
	}
	return "day"
}

// --- Página -----------------------------------------------------------------

// handlePageAdminUsageStats renderiza o painel (form de filtros + KPIs de
// primeira pintura; gráficos carregam via /api/web/admin/usage/stats).
func (a *App) handlePageAdminUsageStats(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}

	f := parseUsageStatsFilter(r)

	type usageStatsContent struct {
		Filter    usageStatsFilter
		FormModel string
		// Sel* = ids selecionados (0 = "todos") para marcar <option selected>.
		SelUserID     int64
		SelKeyID      int64
		SelProviderID int64
		Users         []usageStatsOption
		Keys          []usageStatsOption
		Providers     []usageStatsOption
		Models        []usageStatsOption
		Totals        any // postgres.UsageStatsTotals (nil se DB indisponível)
		Retention     int // dias de retenção dos logs (hint na UI)
	}

	content := usageStatsContent{
		Filter:    f,
		FormModel: f.Model,
		Retention: 90,
	}
	if f.UserID != nil {
		content.SelUserID = *f.UserID
	}
	if f.KeyID != nil {
		content.SelKeyID = *f.KeyID
	}
	if f.ProviderID != nil {
		content.SelProviderID = *f.ProviderID
	}
	content.Users, content.Keys, content.Providers, content.Models = a.usageStatsOptions(r.Context(), sess.OrgID)

	if a.usageRepo != nil {
		if totals, err := a.usageRepo.QueryUsageStatsTotals(r.Context(), f.toPostgresFilters(sess.OrgID)); err == nil {
			content.Totals = totals
		}
	}

	data := a.basePageData(r, "Estatísticas — AI Provider Pool", "usage")
	data.Content = content
	if err := a.renderer.renderPage(w, "pages/usage_stats.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// --- JSON -------------------------------------------------------------------

// usageStatsBreakdownJSON é o payload de um breakdown por dimensão.
type usageStatsBreakdownJSON struct {
	Dimension string                   `json:"dimension"`
	Rows      []usageStatsBreakdownRow `json:"rows"`
}

type usageStatsBreakdownRow struct {
	Label        string  `json:"label"`
	RequestCount int64   `json:"request_count"`
	TotalTokens  int64   `json:"total_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	ErrorCount   int64   `json:"error_count"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}

type usageStatsPointJSON struct {
	Bucket       time.Time `json:"bucket"`
	RequestCount int64     `json:"request_count"`
	TotalTokens  int64     `json:"total_tokens"`
	CachedTokens int64     `json:"cached_tokens"`
	ErrorCount   int64     `json:"error_count"`
}

type usageStatsDataResponse struct {
	From       time.Time                 `json:"from"`
	To         time.Time                 `json:"to"`
	Bucket     string                    `json:"bucket"`
	Totals     postgres.UsageStatsTotals `json:"totals"`
	Series     []usageStatsPointJSON     `json:"series"`
	Breakdowns []usageStatsBreakdownJSON `json:"breakdowns"`
	Filters    usageStatsFiltersApplied  `json:"filters"`
}

type usageStatsFiltersApplied struct {
	UserID     int64  `json:"user_id"`
	KeyID      int64  `json:"key_id"`
	ProviderID int64  `json:"provider_id"`
	Model      string `json:"model"`
}

// handleAdminUsageStatsData retorna KPIs + série temporal + breakdowns em um
// payload único (1 fetch por gráfico no client). Sempre 200 com dados vazios
// quando não há uso no período — estado vazio é resultado válido.
func (a *App) handleAdminUsageStatsData(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	if a.usageRepo == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "usage repo not initialized")
		return
	}

	f := parseUsageStatsFilter(r)
	pf := f.toPostgresFilters(sess.OrgID)

	resp := usageStatsDataResponse{
		From:   f.From,
		To:     f.To,
		Bucket: usageStatsBucket(f.From, f.To),
		Filters: usageStatsFiltersApplied{
			Model: f.Model,
		},
	}
	if f.UserID != nil {
		resp.Filters.UserID = *f.UserID
	}
	if f.KeyID != nil {
		resp.Filters.KeyID = *f.KeyID
	}
	if f.ProviderID != nil {
		resp.Filters.ProviderID = *f.ProviderID
	}

	// Breakdown: dimensão única via ?breakdown=user|key|model|provider, ou
	// todas quando ausente/inválido (default "all").
	dims := []string{"user", "key", "model", "provider"}
	switch r.URL.Query().Get("breakdown") {
	case "user", "key", "model", "provider":
		dims = []string{r.URL.Query().Get("breakdown")}
	}

	totals, err := a.usageRepo.QueryUsageStatsTotals(r.Context(), pf)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not load usage stats")
		return
	}
	resp.Totals = totals

	if points, err := a.usageRepo.QueryUsageStatsTimeSeries(r.Context(), pf, resp.Bucket); err == nil {
		resp.Series = make([]usageStatsPointJSON, 0, len(points))
		for _, p := range points {
			resp.Series = append(resp.Series, usageStatsPointJSON{
				Bucket: p.Bucket, RequestCount: p.RequestCount,
				TotalTokens: p.TotalTokens, CachedTokens: p.CachedTokens,
				ErrorCount: p.ErrorCount,
			})
		}
	}
	for _, d := range dims {
		rows, err := a.usageRepo.QueryUsageStatsBreakdown(r.Context(), pf, d)
		if err != nil {
			continue // tolerante: breakdown ausente não derruba o payload
		}
		bd := usageStatsBreakdownJSON{Dimension: d, Rows: make([]usageStatsBreakdownRow, 0, len(rows))}
		for _, row := range rows {
			bd.Rows = append(bd.Rows, usageStatsBreakdownRow{
				Label: row.GroupKey, RequestCount: row.RequestCount,
				TotalTokens: row.TotalTokens, CachedTokens: row.CachedTokens,
				ErrorCount: row.ErrorCount, AvgLatencyMS: row.AvgLatencyMS,
			})
		}
		resp.Breakdowns = append(resp.Breakdowns, bd)
	}

	writeJSON(w, http.StatusOK, resp)
}
