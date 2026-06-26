package admin

import (
	"net/http"
	"strconv"
	"time"
)

// =============================================================================
// Endpoint JSON para o Dashboard admin (F8).
//
// GET /api/web/admin/dashboard?from=&to=&group_by=user|provider|model
//
// Retorna séries + totais para alimentar gráficos ECharts no template
// pages/dashboard.html. Consulta SOMENTE usage_rollups (nunca logs crus) —
// spec §3.4 Backend T3. Frescura máxima: 1h (job de rollup roda a cada 5min).
//
// Cache: handler não cacheia explicitamente (Cache-Control: no-store). Para
// reduzir carga em dashboards com polling agressivo, futuro: cache curto 60s
// no handler ou CDN.
// =============================================================================

// dashboardSeriesPoint é uma entrada de série para ECharts (label + valor).
type dashboardSeriesPoint struct {
	Label        string `json:"label"`
	RequestCount int64  `json:"request_count"`
	PromptTokens int64  `json:"prompt_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	CachedTokens int64  `json:"cached_tokens"`
	ErrorCount   int64  `json:"error_count"`
}

// dashboardDataResponse é o payload do GET /api/web/admin/dashboard.
type dashboardDataResponse struct {
	From    time.Time              `json:"from"`
	To      time.Time              `json:"to"`
	GroupBy string                 `json:"group_by"`
	Series  []dashboardSeriesPoint `json:"series"`
	Totals  dashboardTotalsJSON    `json:"totals"`
	Partial bool                   `json:"partial,omitempty"` // true se dados incompletos
}

// dashboardTotalsJSON é o resumo agregado (KPI cards).
type dashboardTotalsJSON struct {
	RequestCount     int64 `json:"request_count"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"` // KPI destacado (cache de contexto)
	TotalTokens      int64 `json:"total_tokens"`
	ErrorCount       int64 `json:"error_count"`
}

// handleAdminDashboardData retorna JSON agregado para os gráficos do dashboard.
//
// Query params:
//   - from (RFC3339, default: -7d)
//   - to (RFC3339, default: now)
//   - group_by (user|provider|model, default: user)
//
// Acessível apenas a role=admin (withAdminAuth no grupo de rotas).
func (a *App) handleAdminDashboardData(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	if a.usageRepo == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "usage repo not initialized")
		return
	}

	from, to := parseDashboardWindow(r)
	groupBy := sanitizeGroupBy(r.URL.Query().Get("group_by"))

	rows, totals, err := a.usageRepo.QueryRollupsForDashboard(r.Context(), sess.OrgID, from, to, groupBy)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not load dashboard data")
		return
	}

	// Converte RollupRow → dashboardSeriesPoint (camelCase para JS).
	series := make([]dashboardSeriesPoint, 0, len(rows))
	for _, row := range rows {
		series = append(series, dashboardSeriesPoint{
			Label:        row.GroupKey,
			RequestCount: row.RequestCount,
			PromptTokens: row.PromptTokens,
			TotalTokens:  row.TotalTokens,
			CachedTokens: row.CachedTokens,
			ErrorCount:   row.ErrorCount,
		})
	}

	writeJSON(w, http.StatusOK, dashboardDataResponse{
		From:    from,
		To:      to,
		GroupBy: groupBy,
		Series:  series,
		Totals: dashboardTotalsJSON{
			RequestCount:     totals.RequestCount,
			PromptTokens:     totals.PromptTokens,
			CompletionTokens: totals.CompletionTokens,
			CachedTokens:     totals.CachedTokens,
			TotalTokens:      totals.TotalTokens,
			ErrorCount:       totals.ErrorCount,
		},
	})
}

// parseDashboardWindow lê from/to dos query params com defaults defensivos.
// Formato aceito: RFC3339 (ex.: 2026-06-01T00:00:00Z). Em erro de parse,
// cai para default (-7d → now) sem rejeitar a request.
func parseDashboardWindow(r *http.Request) (time.Time, time.Time) {
	to := time.Now()
	from := to.AddDate(0, 0, -7)
	if v := r.URL.Query().Get("from"); v != "" {
		if parsed, err := time.Parse(time.RFC3339, v); err == nil {
			from = parsed
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if parsed, err := time.Parse(time.RFC3339, v); err == nil {
			to = parsed
		}
	}
	// Garantia: from < to (defesa contra injeção manual de params invertidos).
	if !from.Before(to) {
		from = to.AddDate(0, 0, -7)
	}
	return from, to
}

// sanitizeGroupBy normaliza o parâmetro group_by. Default "user".
func sanitizeGroupBy(v string) string {
	switch v {
	case "user", "provider", "model":
		return v
	default:
		return "user"
	}
}

// parseLimitOffset helper compartilhado para handlers paginados.
// Defaults: limit=50, offset=0. Clamp: limit ∈ [1,200], offset ≥ 0.
func parseLimitOffset(r *http.Request) (int, int) {
	limit := 50
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			offset = n
		}
	}
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
