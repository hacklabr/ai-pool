package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Handlers HTML que renderizam templates server-rendered (F8).
//
// Cada handler:
//  1. Carrega a sessão do contexto (populada por sessionAuthMiddleware).
//  2. Busca dados específicos da página (queries read-only nos repos).
//  3. Monta pageData e renderiza via a.renderer.renderPage.
//
// Convenções:
//   - Admin pages em /web/admin/* exigem role=admin (aplicado via grupo no
//     Handler() com requireRoleMiddleware).
//   - Todas as páginas recebem CSRFToken no contexto para forms HTMX.
//   - Erros de renderer são 500 graceful (não crasham o pod).
//   - Erros de DB são 503 com página de erro amigável (parcial ui/error).
// =============================================================================

// requireRenderer retorna 503 se o renderer não está disponível. Helper usado
// no início de cada handler HTML — evita nil-panic se SetTemplates não foi
// chamado (ex.: testes, dev sem embed.FS).
func (a *App) requireRenderer(w http.ResponseWriter) bool {
	if a.renderer == nil {
		http.Error(w, "UI templates not initialized", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// basePageData monta pageData com campos comuns (User, CSRF, DevMode) extraídos
// da sessão no contexto. Helper para evitar boilerplate em cada handler.
func (a *App) basePageData(r *http.Request, title, activeNav string) pageData {
	sess := sessionFromContext(r.Context())
	csrf := ""
	if sess != nil {
		csrf = sess.CSRFToken
	}
	return pageData{
		Title:     title,
		User:      sess,
		CSRFToken: csrf,
		ActiveNav: activeNav,
		DevMode:   a.devMode,
	}
}

// --- Dashboard (admin) ------------------------------------------------------

// handlePageDashboard renderiza o dashboard admin (KPIs + gráficos ECharts).
//
// Carrega:
//   - KPI totals (RequestCount, CachedTokens, ErrorCount, etc.) via rollups.
//   - Series para os 3 gráficos: por usuário, por provedor, por modelo.
//
// O JS do template (ECharts via CDN) consome os mesmos dados injetados no HTML
// (server-rendered para primeira pintura) e faz polling opcional via
// /api/web/admin/dashboard para atualização.
//
// User comum que acessa /web/dashboard é redirecionado para /web/me/usage.
func (a *App) handlePageDashboard(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}
	// User comum → seu próprio dashboard.
	if domain.Role(sess.Role) != domain.RoleAdmin {
		http.Redirect(w, r, "/web/me/usage", http.StatusSeeOther)
		return
	}

	// Janela padrão: últimos 7 dias.
	to := time.Now()
	from := to.AddDate(0, 0, -7)

	type dashboardContent struct {
		From          time.Time
		To            time.Time
		Totals        any // postgres.DashboardTotals — qualquer tipo para o template
		SeriesByUser  any // []postgres.DashboardRollupRow
		SeriesByProv  any
		SeriesByModel any
	}

	content := dashboardContent{From: from, To: to}

	// Tolerante a erros de DB: se falhar, mostramos zeros e toast de erro.
	if a.usageRepo != nil {
		if totals, err := a.usageRepo.QueryRollupsTotals(r.Context(), sess.OrgID, from, to); err == nil {
			content.Totals = totals
		}
		if rows, _, err := a.usageRepo.QueryRollupsForDashboard(r.Context(), sess.OrgID, from, to, "user"); err == nil {
			content.SeriesByUser = rows
		}
		if rows, _, err := a.usageRepo.QueryRollupsForDashboard(r.Context(), sess.OrgID, from, to, "provider"); err == nil {
			content.SeriesByProv = rows
		}
		if rows, _, err := a.usageRepo.QueryRollupsForDashboard(r.Context(), sess.OrgID, from, to, "model"); err == nil {
			content.SeriesByModel = rows
		}
	}

	data := a.basePageData(r, "Dashboard — AI Provider Pool", "dashboard")
	data.Content = content
	if err := a.renderer.renderPage(w, "pages/dashboard.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// --- Meus tokens ------------------------------------------------------------

// handlePageMyTokens renderiza a lista de tokens do próprio usuário com botão
// "Gerar novo token" (HTMX → POST /api/web/me/tokens mostra sk-aipp-... UMA vez).
//
// Carrega a lista mascarada via tokenRepo.ListByUser. O template lida com o
// estado de "token recém-criado" via flash (HX-Trigger event) — não persistimos
// o valor completo em nenhum lugar além da resposta imediata do POST.
func (a *App) handlePageMyTokens(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}

	type myTokensContent struct {
		Tokens []tokenListItemPublic
	}
	content := myTokensContent{}
	if items, err := a.tokenRepo.ListByUser(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(sess.UserID)); err == nil {
		// Converte para público (mascarado) — reutiliza o tipo do JSON handler.
		for _, it := range items {
			content.Tokens = append(content.Tokens, tokenListItemPublic{
				ID:         int64(it.ID),
				KeyID:      it.KeyID,
				MaskedFull: domain.MaskedToken(it.KeyID),
				Name:       it.Name,
				Status:     string(it.Status),
				CreatedAt:  it.CreatedAt,
				ExpiresAt:  it.ExpiresAt,
				LastUsedAt: it.LastUsedAt,
			})
		}
	}

	data := a.basePageData(r, "Meus tokens — AI Provider Pool", "my-tokens")
	data.Content = content
	if err := a.renderer.renderPage(w, "pages/my_tokens.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// --- Meu uso ----------------------------------------------------------------

// handlePageMyUsage renderiza o consumo do próprio usuário (visão pessoal).
//
// Reusa a infraestrutura do dashboard admin mas escopada por user_id. KPIs:
// tokens consumidos, tokens economizados por cache, requisições, taxa de erro.
// Gráfico ECharts donut de distribuição por modelo.
func (a *App) handlePageMyUsage(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}

	to := time.Now()
	from := to.AddDate(0, 0, -7)

	type myUsageContent struct {
		From          time.Time
		To            time.Time
		Totals        any
		SeriesByModel any
	}
	content := myUsageContent{From: from, To: to}

	if a.usageRepo != nil {
		// Tempo real (usage_logs): a tela "Meu uso" faz polling de 10s, então
		// lemos logs direto (índice user-time) em vez de rollups (que lagam ~1min).
		if totals, err := a.usageRepo.QueryUserUsageTotalsFromLogs(r.Context(), sess.OrgID, sess.UserID, from, to); err == nil {
			content.Totals = totals
		}
		if rows, err := a.usageRepo.QueryUserUsageByModelFromLogs(r.Context(), sess.OrgID, sess.UserID, from, to); err == nil {
			content.SeriesByModel = rows
		}
	}

	data := a.basePageData(r, "Meu uso — AI Provider Pool", "my-usage")
	data.Content = content
	if err := a.renderer.renderPage(w, "pages/my_usage.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// --- Admin: Usuários --------------------------------------------------------

// handlePageAdminUsers renderiza a gestão de usuários (CRUD admin).
//
// Lista todos os usuários da org (sem password_hash). Inline edit via HTMX:
// PATCH /api/web/admin/users/{id}/status para habilitar/desabilitar. Criação
// via modal HTMX: POST /api/web/admin/users.
func (a *App) handlePageAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}

	type usersContent struct {
		Users []userAdminListItem
	}
	content := usersContent{}

	rows, err := a.pool.Query(r.Context(), `
		SELECT id, organization_id, username, role, status, disabled_at, created_at, updated_at, last_login_at
		FROM users
		WHERE organization_id = $1
		ORDER BY username`, sess.OrgID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var u userAdminListItem
			var lastLogin *time.Time
			if err := rows.Scan(&u.ID, &u.OrgID, &u.Username, &u.Role, &u.Status, &u.DisabledAt, &u.CreatedAt, &u.UpdatedAt, &lastLogin); err == nil {
				u.LastLogin = lastLogin
				content.Users = append(content.Users, u)
			}
		}
	}

	data := a.basePageData(r, "Usuários — AI Provider Pool", "users")
	data.Content = content
	if err := a.renderer.renderPage(w, "pages/users.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// --- Admin: Provedores ------------------------------------------------------

// handlePageAdminProviders renderiza a gestão de provedores (CRUD admin).
//
// Lista providers ativos da org. Cada provider é um card com base_url,
// User-Agent override global e count de chaves vinculadas. Criação via modal.
func (a *App) handlePageAdminProviders(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}

	type providersContent struct {
		Providers []providerListItem
	}
	content := providersContent{}

	if provs, err := a.providerRepo.ListAll(r.Context(), domain.OrganizationID(sess.OrgID)); err == nil {
		for _, p := range provs {
			content.Providers = append(content.Providers, providerListItem{
				ID:                int64(p.ID),
				Name:              p.Name,
				Slug:              p.Slug,
				BaseURL:           p.BaseURL,
				APISpec:           p.APISpec,
				UserAgentOverride: p.UserAgentGlobal,
				IsActive:          p.IsActive,
				CreatedAt:         p.CreatedAt,
			})
		}
	}

	data := a.basePageData(r, "Provedores — AI Provider Pool", "providers")
	data.Content = content
	if err := a.renderer.renderPage(w, "pages/providers.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// --- Admin: Chaves ----------------------------------------------------------

// handlePageAdminKeys renderiza a gestão de chaves (a tela mais complexa).
//
// Lista todas as chaves agrupadas por provider, com 4 estados visuais:
//   - enabled (verde) — ativa, no pool.
//   - manual_disabled (cinza) — admin desativou; só admin reativa.
//   - auto_disabled (amarelo) — sistema desativou; mostra data de reativação.
//   - em backoff (laranja) — derivado de error_count alto + disabled_until
//     futuro (tratado no template como sub-estado de auto_disabled).
//
// Ações por chave: force-enable/disable (POST HTMX), ver histórico de erros
// (futuro), editar User-Agent override (futuro). Force-enable retorna a linha
// re-renderizada via HX-Trigger.
func (a *App) handlePageAdminKeys(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}

	type keysContent struct {
		Keys         []keyListItem
		Providers    map[int64]string               // id → slug para agrupamento visual
		ProviderList []providerListItem             // id+name+slug ordenados p/ o <select> do form de criação
		KeyUsage     map[int64]postgres.KeyUsageRow // key_id → uso global (7d)
	}
	content := keysContent{Providers: map[int64]string{}, KeyUsage: map[int64]postgres.KeyUsageRow{}}

	// Uso global por chave (7d) — exibido como colunas na lista de chaves.
	if a.usageRepo != nil {
		if rows, err := a.usageRepo.QueryKeyUsageFromLogs(r.Context(), sess.OrgID, 0,
			time.Now().AddDate(0, 0, -7), time.Now()); err == nil {
			for _, kr := range rows {
				content.KeyUsage[int64(kr.KeyID)] = kr
			}
		}
	}

	// Carrega slugs de providers para o agrupamento visual e para o <select>.
	if provs, err := a.providerRepo.ListActive(r.Context(), domain.OrganizationID(sess.OrgID)); err == nil {
		for _, p := range provs {
			content.Providers[int64(p.ID)] = p.Slug
			content.ProviderList = append(content.ProviderList, providerListItem{
				ID:   int64(p.ID),
				Name: p.Name,
				Slug: p.Slug,
			})
		}
	}

	// Carrega chaves (flat list; agrupamento visual no template).
	rows, err := a.pool.Query(r.Context(), `
		SELECT id, provider_id, label, status, disabled_reason, disabled_until,
		       error_count, last_used_at, user_agent_override,
		       plan_name, TO_CHAR(plan_renewal_date, 'YYYY-MM-DD'), plan_quota_tokens,
		       created_at
		FROM provider_keys
		WHERE organization_id = $1
		ORDER BY provider_id, label`, sess.OrgID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var k keyListItem
			var disabledReason, uaOverride, planName, planRenewal *string
			var planQuota *int64
			if err := rows.Scan(&k.ID, &k.ProviderID, &k.Label, &k.Status, &disabledReason,
				&k.DisabledUntil, &k.ErrorCount, &k.LastUsedAt, &uaOverride,
				&planName, &planRenewal, &planQuota, &k.CreatedAt); err == nil {
				k.DisabledReason = disabledReason
				k.UserAgentOverride = uaOverride
				k.PlanName = planName
				k.PlanRenewalDate = planRenewal
				k.PlanQuotaTokens = planQuota
				content.Keys = append(content.Keys, k)
			}
		}
	}

	data := a.basePageData(r, "Chaves — AI Provider Pool", "keys")
	data.Content = content
	if err := a.renderer.renderPage(w, "pages/keys.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// --- Admin: Tokens ----------------------------------------------------------

// handlePageAdminTokens renderiza a gestão de tokens de API (admin).
//
// Lista todos os tokens da org com user_id/username, status, e ações de
// revogar/deletar. Carrega a lista via API /api/web/admin/tokens (HTMX/Alpine).
func (a *App) handlePageAdminTokens(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}

	data := a.basePageData(r, "Tokens — AI Provider Pool", "tokens")
	data.Content = struct{}{}
	if err := a.renderer.renderPage(w, "pages/tokens.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// --- Admin: Auditoria -------------------------------------------------------

// handlePageAudit renderiza a tela de auditoria (read-only).
//
// A lista paginada é carregada via HTMX chamando /api/web/admin/audit (JSON),
// não server-rendered na primeira carga. Isto porque:
//   - Auditoria pode ter milhares de entradas — não queremos tudo no HTML inicial.
//   - Paginação via HTMX + Alpine é mais fluida para logs longos.
//   - Mantém o handler de página leve (apenas shell + container HTMX).
//
// O template renderiza o container com hx-get="/api/web/admin/audit?limit=50"
// no carregamento. O handler JSON retorna o HTML de uma página (partial) ou
// JSON puro consumido por Alpine — escolhemos JSON + Alpine para máxima
// flexibilidade de filtros.
func (a *App) handlePageAdminAudit(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}

	// Carrega primeira página para SSR (boa UX: conteúdo visível no load).
	type auditContent struct {
		Entries []auditEntryUI
		Total   int
		Limit   int
		Offset  int
		Actions []string // ações distintas para o filtro (TODO: query DISTINCT)
	}
	content := auditContent{Limit: 50, Offset: 0}

	if a.auditRepo != nil {
		entries, total, err := a.auditRepo.List(r.Context(), sess.OrgID, postgresAuditFilterFromQuery(r), 50, 0)
		if err == nil {
			content.Total = total
			for _, e := range entries {
				content.Entries = append(content.Entries, auditEntryUI{
					ID:          e.ID,
					ActorUserID: e.ActorUserID,
					ActorName:   e.ActorName,
					Action:      e.Action,
					EntityType:  e.EntityType,
					EntityID:    e.EntityID,
					Metadata:    string(e.Metadata),
					IP:          e.IP,
					UserAgent:   e.UserAgent,
					CreatedAt:   e.CreatedAt,
				})
			}
		}
	}

	data := a.basePageData(r, "Auditoria — AI Provider Pool", "audit")
	data.Content = content
	if err := a.renderer.renderPage(w, "pages/audit.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// postgresAuditFilterFromQuery monta AuditFilter a partir dos query params.
// Helper reusável entre o handler HTML e o JSON.
func postgresAuditFilterFromQuery(r *http.Request) postgresAuditFilterView {
	// Delegado para audit_web.go via tipo compartilhado. Aqui apenas chamamos.
	return parseAuditFilter(r)
}

// parseAuditFilter lê query params opcionais (action, entity_type, from, to,
// actor) e monta o filtro. Strings vazias viram "sem filtro".
func parseAuditFilter(r *http.Request) postgresAuditFilterView {
	q := r.URL.Query()
	f := postgresAuditFilterView{}
	if v := strings.TrimSpace(q.Get("action")); v != "" {
		f.Action = v
	}
	if v := strings.TrimSpace(q.Get("entity_type")); v != "" {
		f.EntityType = v
	}
	if v := strings.TrimSpace(q.Get("actor")); v != "" {
		// actor é username — converter para ID seria necessário. No MVP,
		// suportamos apenas filtro por action/entity_type para simplicidade.
		// O filtro por actor fica como roadmap.
		_ = v
	}
	return f
}
