package admin

import (
	"net/http"
	"time"
)

// =============================================================================
// "Meu uso" — endpoint JSON para auto-atualização por polling (setInterval 10s).
//
// Lê usage_logs em TEMPO REAL (QueryUserUsageTotalsFromLogs /
// QueryUserUsageByModelFromLogs) para que cada poll reflita as últimas
// requisições (frescor ~10s), ao invés dos rollups (lag ~1min do job).
//
// GET /api/web/me/usage — só session auth (GET não exige CSRF). A flag
// `has_data` indica se há totais (para o JS saber se mostra o estado vazio).
// =============================================================================

// handleMyUsageData retorna KPIs pessoais + série por modelo em JSON.
func (a *App) handleMyUsageData(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	to := time.Now()
	from := to.AddDate(0, 0, -7)

	resp := myUsageDataResponse{From: from, To: to}
	if a.usageRepo != nil {
		if totals, err := a.usageRepo.QueryUserUsageTotalsFromLogs(r.Context(), sess.OrgID, sess.UserID, from, to); err == nil {
			resp.Totals = myUsageTotalsJSON{
				RequestCount:     totals.RequestCount,
				PromptTokens:     totals.PromptTokens,
				CompletionTokens: totals.CompletionTokens,
				CachedTokens:     totals.CachedTokens,
				TotalTokens:      totals.TotalTokens,
				NonCachedTokens:  totals.NonCachedTokens,
				ErrorCount:       totals.ErrorCount,
			}
			resp.HasData = true
		}
		if rows, err := a.usageRepo.QueryUserUsageByModelFromLogs(r.Context(), sess.OrgID, sess.UserID, from, to); err == nil {
			for _, row := range rows {
				resp.SeriesByModel = append(resp.SeriesByModel, myUsageModelJSON{
					GroupKey:    row.GroupKey,
					TotalTokens: row.TotalTokens,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type myUsageDataResponse struct {
	From          time.Time          `json:"from"`
	To            time.Time          `json:"to"`
	HasData       bool               `json:"has_data"`
	Totals        myUsageTotalsJSON  `json:"totals"`
	SeriesByModel []myUsageModelJSON `json:"series_by_model"`
}

type myUsageTotalsJSON struct {
	RequestCount     int64 `json:"request_count"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	NonCachedTokens  int64 `json:"non_cached_tokens"`
	ErrorCount       int64 `json:"error_count"`
}

type myUsageModelJSON struct {
	GroupKey    string `json:"group_key"`
	TotalTokens int64  `json:"total_tokens"`
}
