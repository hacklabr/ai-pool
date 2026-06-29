package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Handlers admin de gestão de MODELOS (tela ausente em F8 — adicionada agora).
//
// Um modelo é o nome que o cliente envia no campo `model` do payload OpenAI
// (ex.: "glm-4.6"). O routing engine resolve model→provider→keys, logo um
// modelo precisa estar cadastrado para o proxy rotear. O cadastro associa um
// nome de modelo a um provider existente.
//
// Operações:
//
//   - GET  /api/web/admin/models        → lista modelos da org (com provider).
//   - POST /api/web/admin/models        → cria modelo vinculado a um provider.
//   - GET  /web/admin/models            → página HTML (lista + form de criação).
//
// Acesso exclusivo de role=admin (withAdminAuth).
// =============================================================================

// modelListItem para listagem admin (sem dados sensíveis; modelos não têm).
type modelListItem struct {
	ID            int64     `json:"id"`
	ProviderID    int64     `json:"provider_id"`
	ProviderName  string    `json:"provider_name"`
	ProviderSlug  string    `json:"provider_slug"`
	Name          string    `json:"name"`
	DisplayName   *string   `json:"display_name,omitempty"`
	ContextWindow *int      `json:"context_window,omitempty"`
	IsActive      bool      `json:"is_active"`
	CreatedAt     time.Time `json:"created_at"`
}

// createModelRequest payload de POST /api/web/admin/models.
//
// ContextWindow é *int para permitir "vazio = não informado" (decodeForm pula
// campos vazios, deixando o ponteiro nil). ProviderID é obrigatório.
type createModelRequest struct {
	ProviderID    int64  `json:"provider_id"`
	Name          string `json:"name"`           // ex.: "glm-4.6"
	DisplayName   string `json:"display_name"`   // opcional (rótulo amigável)
	ContextWindow *int   `json:"context_window"` // opcional (tokens)
}

// listModelsQuery carrega modelos da org com dados do provider (para lista e
// para a página HTML). Retorna slice vazio (não nil) se não houver modelos.
func (a *App) listModelsQuery(r *http.Request, orgID int64) []modelListItem {
	out := make([]modelListItem, 0)
	rows, err := a.pool.Query(r.Context(), `
		SELECT m.id, m.provider_id, p.name, p.slug, m.name,
		       m.display_name, m.context_window, m.is_active, m.created_at
		FROM models m
		JOIN providers p ON p.id = m.provider_id
		WHERE m.organization_id = $1
		ORDER BY p.slug, m.name`, orgID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var m modelListItem
		var displayName *string
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.ProviderName, &m.ProviderSlug,
			&m.Name, &displayName, &m.ContextWindow, &m.IsActive, &m.CreatedAt); err == nil {
			m.DisplayName = displayName
			out = append(out, m)
		}
	}
	return out
}

// handleAdminListModels retorna todos os modelos da org (JSON).
func (a *App) handleAdminListModels(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	models := a.listModelsQuery(r, sess.OrgID)
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// handleAdminCreateModel cria um novo modelo vinculado a um provider da org.
func (a *App) handleAdminCreateModel(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	var req createModelRequest
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.ProviderID <= 0 || req.Name == "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "provider_id and name are required")
		return
	}

	// Valida que o provider existe e está ativo na org (evita FK opaca e dá
	// mensagem amigável). Reusa ListActive (cache de providers é pequeno).
	provs, err := a.providerRepo.ListActive(r.Context(), domain.OrganizationID(sess.OrgID))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not validate provider")
		return
	}
	providerOK := false
	for _, p := range provs {
		if int64(p.ID) == req.ProviderID {
			providerOK = true
			break
		}
	}
	if !providerOK {
		writeAPIError(w, http.StatusBadRequest, "provider_not_found", "provider not found or inactive")
		return
	}

	m := &domain.Model{
		ProviderID: domain.ID(req.ProviderID),
		OrgID:      domain.OrganizationID(sess.OrgID),
		Name:       req.Name,
		IsActive:   true,
	}
	if req.DisplayName != "" {
		m.DisplayName = &req.DisplayName
	}
	m.ContextWindow = req.ContextWindow // nil se não informado

	id, err := postgres.NewModelRepo(a.pool).Create(r.Context(), m)
	if err != nil {
		if isUniqueViolation(err) {
			writeAPIError(w, http.StatusConflict, "model_exists", "model name already exists for this provider")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not create model")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          int64(id),
		"provider_id": req.ProviderID,
		"name":        req.Name,
	})
}

// --- Admin: Modelos (página HTML) -------------------------------------------

// handlePageAdminModels renderiza a gestão de modelos (lista + form de criação).
//
// Carrega:
//   - modelos da org (com provider) para a lista.
//   - providers ativos para o <select> do form de criação.
func (a *App) handlePageAdminModels(w http.ResponseWriter, r *http.Request) {
	if !a.requireRenderer(w) {
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/web/login", http.StatusSeeOther)
		return
	}

	type modelsContent struct {
		Models    []modelListItem
		Providers []providerListItem // para o <select> do form de criação
	}
	content := modelsContent{
		Models: a.listModelsQuery(r, sess.OrgID),
	}
	if provs, err := a.providerRepo.ListActive(r.Context(), domain.OrganizationID(sess.OrgID)); err == nil {
		for _, p := range provs {
			content.Providers = append(content.Providers, providerListItem{
				ID:   int64(p.ID),
				Name: p.Name,
				Slug: p.Slug,
			})
		}
	}

	data := a.basePageData(r, "Modelos — AI Provider Pool", "models")
	data.Content = content
	if err := a.renderer.renderPage(w, "pages/models.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}
