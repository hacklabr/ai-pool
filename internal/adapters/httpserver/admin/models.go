package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
)

// jsonHasImage retorna true se o JSONB modalities contém "image".
func jsonHasImage(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	var mods []string
	if err := json.Unmarshal(b, &mods); err != nil {
		return false
	}
	for _, m := range mods {
		if m == "image" {
			return true
		}
	}
	return false
}

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
	OutputLimit   *int      `json:"output_limit,omitempty"`
	Multimodal    bool      `json:"multimodal"`
	IsActive      bool      `json:"is_active"`
	CreatedAt     time.Time `json:"created_at"`
}

// createModelRequest payload de POST /api/web/admin/models.
type createModelRequest struct {
	ProviderID    int64  `json:"provider_id"`
	Name          string `json:"name"`
	DisplayName   string `json:"display_name"`
	ContextWindow *int   `json:"context_window"`
	OutputLimit   *int   `json:"output_limit"`
	Multimodal    bool   `json:"multimodal"`
}

// listModelsQuery carrega modelos da org com dados do provider (para lista e
// para a página HTML). Retorna slice vazio (não nil) se não houver modelos.
func (a *App) listModelsQuery(r *http.Request, orgID int64) []modelListItem {
	out := make([]modelListItem, 0)
	rows, err := a.pool.Query(r.Context(), `
		SELECT m.id, m.provider_id, p.name, p.slug, m.name,
		       m.display_name, m.context_window, m.output_limit, m.modalities, m.is_active, m.created_at
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
		var modalitiesJSON []byte
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.ProviderName, &m.ProviderSlug,
			&m.Name, &displayName, &m.ContextWindow, &m.OutputLimit, &modalitiesJSON, &m.IsActive, &m.CreatedAt); err == nil {
			m.DisplayName = displayName
			m.Multimodal = jsonHasImage(modalitiesJSON)
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
	m.ContextWindow = req.ContextWindow
	m.OutputLimit = req.OutputLimit
	if req.Multimodal {
		m.Modalities = []string{"text", "image"}
	}

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

// updateModelRequest payload de PATCH /api/web/admin/models/{id}.
type updateModelRequest struct {
	Name          string `json:"name"`
	DisplayName   string `json:"display_name"`
	ContextWindow *int   `json:"context_window"`
	OutputLimit   *int   `json:"output_limit"`
	IsActive      bool   `json:"is_active"`
	Multimodal    bool   `json:"multimodal"`
}

func modelFromPath(r *http.Request) (int64, bool) {
	return pathID(r.URL.Path, "/api/web/admin/models/")
}

// handleAdminGetModel retorna um modelo pelo ID.
func (a *App) handleAdminGetModel(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := modelFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid model id")
		return
	}
	m, err := postgres.NewModelRepo(a.pool).GetByID(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id))
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "model not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not load model")
		return
	}
	p, _ := a.providerRepo.GetByID(r.Context(), domain.OrganizationID(sess.OrgID), m.ProviderID)
	pName := ""
	pSlug := ""
	if p != nil {
		pName = p.Name
		pSlug = p.Slug
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model": modelListItem{
			ID:            int64(m.ID),
			ProviderID:    int64(m.ProviderID),
			ProviderName:  pName,
			ProviderSlug:  pSlug,
			Name:          m.Name,
			DisplayName:   m.DisplayName,
			ContextWindow: m.ContextWindow,
			OutputLimit:   m.OutputLimit,
			Multimodal:    m.IsMultimodal(),
			IsActive:      m.IsActive,
			CreatedAt:     m.CreatedAt,
		},
	})
}

// handleAdminUpdateModel atualiza um modelo.
func (a *App) handleAdminUpdateModel(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := modelFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid model id")
		return
	}
	var req updateModelRequest
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.Name == "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	m := &domain.Model{
		ID:            domain.ID(id),
		OrgID:         domain.OrganizationID(sess.OrgID),
		Name:          req.Name,
		IsActive:      req.IsActive,
		ContextWindow: req.ContextWindow,
		OutputLimit:   req.OutputLimit,
	}
	if req.DisplayName != "" {
		m.DisplayName = &req.DisplayName
	}
	if req.Multimodal {
		m.Modalities = []string{"text", "image"}
	}
	if err := postgres.NewModelRepo(a.pool).Update(r.Context(), domain.OrganizationID(sess.OrgID), m); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "model not found")
			return
		}
		if errors.Is(err, postgres.ErrConflict) {
			writeAPIError(w, http.StatusConflict, "model_exists", "model name already exists for this provider")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not update model")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleAdminToggleModelActive ativa/inativa um modelo.
func (a *App) handleAdminToggleModelActive(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := modelFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid model id")
		return
	}
	var req struct {
		Active bool `json:"active"`
	}
	if err := parseJSON(r, &req, 1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid body")
		return
	}
	if err := postgres.NewModelRepo(a.pool).SetActive(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id), req.Active); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "model not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not update model")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"active": req.Active})
}

// handleAdminDeleteModel remove um modelo fisicamente.
func (a *App) handleAdminDeleteModel(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := modelFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid model id")
		return
	}
	if err := postgres.NewModelRepo(a.pool).Delete(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "model not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not delete model")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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
