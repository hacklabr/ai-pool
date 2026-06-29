package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/observability"
)

// =============================================================================
// Handlers admin de gestão de providers e keys (esboço de F4).
//
// Operações mínimas funcionais — UI completa é F8. Esta fase entrega:
//
//   - GET    /api/web/admin/providers        → lista providers ativos.
//   - POST   /api/web/admin/providers        → cria provider (com SSRF guard).
//   - GET    /api/web/admin/keys             → lista keys (todas, não só usable).
//   - POST   /api/web/admin/keys             → cria key (cifragem real é F7;
//                                              F4 guarda valor direto em DEV).
//   - POST   /api/web/admin/keys/{id}/force-disable  → disable manual.
//   - POST   /api/web/admin/keys/{id}/force-enable   → reativa manual.
//
// Acesso exclusivo de role=admin (withAdminAuth).
//
// Segurança:
//   - base_url validada via domain.ValidateProviderURL (SSRF guard).
//   - encrypted_value é tratado como opaque byte slice; em DEV (IdentityDecryptor)
//     assume-se plaintext; em F7 será envelope-encrypted no caminho de create.
// =============================================================================

// providerListItem para listagem admin.
type providerListItem struct {
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	Slug              string    `json:"slug"`
	BaseURL           string    `json:"base_url"`
	APISpec           string    `json:"api_spec"`
	UserAgentOverride *string   `json:"user_agent_override,omitempty"`
	IsActive          bool      `json:"is_active"`
	CreatedAt         time.Time `json:"created_at"`
}

// createProviderRequest payload de POST /api/web/admin/providers.
type createProviderRequest struct {
	Name              string  `json:"name"`
	Slug              string  `json:"slug"`
	BaseURL           string  `json:"base_url"`
	APISpec           string  `json:"api_spec"`            // default: openai
	UserAgentOverride *string `json:"user_agent_override"` // opcional
}

// handleAdminListProviders retorna todos os providers ativos da org.
func (a *App) handleAdminListProviders(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	provs, err := a.providerRepo.ListActive(r.Context(), domain.OrganizationID(sess.OrgID))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not list providers")
		return
	}
	out := make([]providerListItem, 0, len(provs))
	for _, p := range provs {
		out = append(out, providerListItem{
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
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

// handleAdminCreateProvider cria um novo provider após validar SSRF.
func (a *App) handleAdminCreateProvider(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	var req createProviderRequest
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	if req.Name == "" || req.Slug == "" || req.BaseURL == "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "name, slug and base_url are required")
		return
	}
	if req.APISpec == "" {
		req.APISpec = "openai"
	}

	// SSRF guard: valida scheme + bloqueia IPs privados/loopback/metadata.
	// allowInsecureHTTP respeita config (DEV true, PROD false via Validate).
	if err := domain.ValidateProviderURL(req.BaseURL, domain.WithSSRFAllowInsecureHTTP(!a.cookieCfg.Secure)); err != nil {
		if domain.IsSSRFBlocked(err) {
			writeAPIError(w, http.StatusBadRequest, "ssrf_blocked", "base_url blocked by SSRF guard")
			return
		}
		// Unresolvable pode ser temporário — reportamos como bad request para
		// o admin corrigir/retentar.
		writeAPIError(w, http.StatusBadRequest, "dns_error", "could not resolve base_url host")
		return
	}

	p := &domain.Provider{
		OrgID:           domain.OrganizationID(sess.OrgID),
		Name:            req.Name,
		Slug:            req.Slug,
		BaseURL:         req.BaseURL,
		APISpec:         req.APISpec,
		UserAgentGlobal: req.UserAgentOverride,
		IsActive:        true,
	}
	id, err := a.providerRepo.Create(r.Context(), p)
	if err != nil {
		if isUniqueViolation(err) {
			writeAPIError(w, http.StatusConflict, "slug_taken", "slug already exists")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not create provider")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":       int64(id),
		"name":     req.Name,
		"slug":     req.Slug,
		"base_url": req.BaseURL,
	})
}

// keyListItem para listagem admin (sem o valor cifrado).
type keyListItem struct {
	ID                int64      `json:"id"`
	ProviderID        int64      `json:"provider_id"`
	Label             string     `json:"label"`
	Status            string     `json:"status"`
	DisabledReason    *string    `json:"disabled_reason,omitempty"`
	DisabledUntil     *time.Time `json:"disabled_until,omitempty"`
	ErrorCount        int        `json:"error_count"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
	UserAgentOverride *string    `json:"user_agent_override,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// createKeyRequest payload de POST /api/web/admin/keys.
//
// NOTA: em F4, o valor é tratado como plaintext e guardado direto (DEV_MODE).
// Em F7 (envelope encryption), este endpoint aplica AES-256-GCM ANTES de
// persistir — nenhuma mudança visível para o admin.
type createKeyRequest struct {
	ProviderID        int64   `json:"provider_id"`
	Label             string  `json:"label"`
	Value             string  `json:"value"` // segredo do provedor (plaintext no request)
	UserAgentOverride *string `json:"user_agent_override,omitempty"`
}

// handleAdminListKeys lista todas as keys da org (incluindo desabilitadas).
//
// Não lista por provider (em F4 não há provider_id no path). Retorna flat list;
// admin UI agrupa por provider via frontend. Em F8 refinaremos com filtro.
func (a *App) handleAdminListKeys(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	rows, err := a.pool.Query(r.Context(), `
		SELECT id, provider_id, label, status, disabled_reason, disabled_until,
		       error_count, last_used_at, user_agent_override, created_at
		FROM provider_keys
		WHERE organization_id = $1
		ORDER BY provider_id, label`, sess.OrgID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not list keys")
		return
	}
	defer rows.Close()

	out := make([]keyListItem, 0)
	for rows.Next() {
		var k keyListItem
		var disabledReason, uaOverride *string
		if err := rows.Scan(&k.ID, &k.ProviderID, &k.Label, &k.Status, &disabledReason,
			&k.DisabledUntil, &k.ErrorCount, &k.LastUsedAt, &uaOverride, &k.CreatedAt); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal", "scan error")
			return
		}
		k.DisabledReason = disabledReason
		k.UserAgentOverride = uaOverride
		out = append(out, k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// handleAdminCreateKey cria uma nova key.
//
// ATENÇÃO: o valor é guardado como plaintext em DEV (IdentityDecryptor).
// Em F7, este handler deve aplicar AES-256-GCM envelope encryption ANTES
// de chamar keyRepo.Create. O ponto de extensão é claro (substituir
// []byte(req.Value) pela saída do encryptor).
func (a *App) handleAdminCreateKey(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	var req createKeyRequest
	if err := parseJSON(r, &req, 64*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.ProviderID <= 0 || req.Label == "" || req.Value == "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "provider_id, label and value are required")
		return
	}
	label := strings.TrimSpace(req.Label)
	if len(label) > 100 {
		label = label[:100]
	}

	// F7: aplicar envelope encryption se o encryptor está configurado.
	// Em DEV_MODE sem encryptor, cai para plaintext (IdentityDecryptor funciona).
	var encryptedValue []byte
	encryptionKeyID := "v1"
	if a.encryptor != nil {
		var err error
		encryptionKeyID, encryptedValue, err = a.encryptor.Encrypt([]byte(req.Value))
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal", "could not encrypt key value")
			return
		}
	} else {
		// DEV fallback: plaintext direto. AVISO: não usar em produção.
		encryptedValue = []byte(req.Value)
	}

	k := &domain.ProviderKey{
		ProviderID:        domain.ID(req.ProviderID),
		OrgID:             domain.OrganizationID(sess.OrgID),
		Label:             label,
		EncryptedValue:    encryptedValue,
		EncryptionKeyID:   encryptionKeyID,
		Status:            domain.KeyStatusEnabled,
		UserAgentOverride: req.UserAgentOverride,
	}
	id, err := a.keyRepo.Create(r.Context(), k)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not create key")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          int64(id),
		"provider_id": req.ProviderID,
		"label":       label,
		"status":      string(domain.KeyStatusEnabled),
	})
}

// handleAdminForceDisableKey aplica disable manual (admin override).
// Ignora disabled_until (manual não tem reativação automática).
func (a *App) handleAdminForceDisableKey(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id := pathKeyID(r.URL.Path)
	if id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid key id")
		return
	}
	if err := a.keyRepo.TransitionToManualDisabled(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "key not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not disable key")
		return
	}
	// Métrica Prometheus (F11): disable manual pelo admin.
	observability.IncKeyDisabled("manual")
	writeJSON(w, http.StatusOK, map[string]string{"status": "manual_disabled"})
}

// handleAdminForceEnableKey reativa manualmente uma key.
// Limpa status, disabled_reason, disabled_until, ZERA error_count e consecutive_errors.
func (a *App) handleAdminForceEnableKey(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id := pathKeyID(r.URL.Path)
	if id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid key id")
		return
	}
	if err := a.keyRepo.Reactivate(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "key not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not enable key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "enabled"})
}

// pathKeyID extrai o {id} de paths /api/web/admin/keys/{id}/{action}.
// Retorna 0 se inválido.
func pathKeyID(p string) int64 {
	rest := strings.TrimPrefix(p, "/api/web/admin/keys/")
	rest = strings.TrimPrefix(rest, "/")
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
