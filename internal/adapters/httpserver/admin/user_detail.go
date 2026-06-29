package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Página de detalhe do usuário (admin).
//
// GET  /web/admin/users/{id}          → página (dados, senha, consumo, gráfico
//                                       horário, quebra por chave).
// POST /api/web/admin/users/{id}/password → admin redefine a senha (Argon2id).
//
// Janela de KPIs: últimos 7 dias. Gráfico horário: últimas 24h. Quebra por
// chave: 7 dias (logs, tempo real).
// =============================================================================

// handlePageAdminUserDetail renderiza o detalhe de um usuário.
func (a *App) handlePageAdminUserDetail(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	user, err := a.userRepo.GetByID(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	to := time.Now()
	from7 := to.AddDate(0, 0, -7)
	from24 := to.AddDate(0, 0, -1) // últimas 24h para o gráfico horário

	type detailContent struct {
		User       *domain.User
		Totals     postgres.DashboardTotals
		Hourly     []postgres.HourlyRow
		KeyUsage   []postgres.KeyUsageRow
		FromHourly time.Time
		ToHourly   time.Time
	}
	content := detailContent{
		User:       user,
		FromHourly: from24,
		ToHourly:   to,
	}
	if a.usageRepo != nil {
		if totals, err := a.usageRepo.QueryUserUsageTotalsFromLogs(r.Context(), sess.OrgID, id, from7, to); err == nil {
			content.Totals = totals
		}
		if hourly, err := a.usageRepo.QueryUserHourlyFromLogs(r.Context(), sess.OrgID, id, from24, to); err == nil {
			content.Hourly = hourly
		}
		if keys, err := a.usageRepo.QueryKeyUsageFromLogs(r.Context(), sess.OrgID, id, from7, to); err == nil {
			content.KeyUsage = keys
		}
	}

	data := a.basePageData(r, user.Username+" — AI Provider Pool", "users")
	data.Content = content
	if err := a.renderer.renderPage(w, "pages/user_detail.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// handleAdminSetUserPassword permite ao admin redefinir a senha de um usuário.
// Política mínima: 8 caracteres (NIST 800-63B). Argon2id.
func (a *App) handleAdminSetUserPassword(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid body")
		return
	}
	req.Password = strings.TrimSpace(req.Password)
	if len(req.Password) < 8 {
		writeAPIError(w, http.StatusBadRequest, "weak_password", "password must be at least 8 characters")
		return
	}
	hash, err := domain.HashPassword([]byte(req.Password))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not hash password")
		return
	}
	if err := a.userRepo.UpdatePassword(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id), []byte(hash)); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not update password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
