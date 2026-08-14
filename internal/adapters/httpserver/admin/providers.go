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
	// Campos de plano (issue #19, RF-01). PlanRenewalDate é YYYY-MM-DD (DATE).
	PlanName        *string   `json:"plan_name,omitempty"`
	PlanRenewalDate *string   `json:"plan_renewal_date,omitempty"`
	PlanQuotaTokens *int64    `json:"plan_quota_tokens,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
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
	// Campos de plano (issue #19, RF-01). Opcionais: chaves pay-as-you-go
	// não têm ciclo de plano.
	PlanName        string  `json:"plan_name"`
	PlanRenewalDate *string `json:"plan_renewal_date,omitempty"` // YYYY-MM-DD
	PlanQuotaTokens *int64  `json:"plan_quota_tokens,omitempty"`
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
		       error_count, last_used_at, user_agent_override,
		       plan_name, TO_CHAR(plan_renewal_date, 'YYYY-MM-DD'), plan_quota_tokens,
		       created_at
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
		var disabledReason, uaOverride, planName, planRenewal *string
		var planQuota *int64
		if err := rows.Scan(&k.ID, &k.ProviderID, &k.Label, &k.Status, &disabledReason,
			&k.DisabledUntil, &k.ErrorCount, &k.LastUsedAt, &uaOverride,
			&planName, &planRenewal, &planQuota, &k.CreatedAt); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal", "scan error")
			return
		}
		k.DisabledReason = disabledReason
		k.UserAgentOverride = uaOverride
		k.PlanName = planName
		k.PlanRenewalDate = planRenewal
		k.PlanQuotaTokens = planQuota
		out = append(out, k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// parsePlanFields valida e converte os campos de plano do payload admin
// (issue #19, RF-01). Semântica: valores vazios/ausentes → nil (sem plano).
// Retorna erro legível quando a validação falha.
func parsePlanFields(planName string, renewalDate *string, quotaTokens *int64) (*string, *time.Time, *int64, string) {
	var renewal *time.Time
	if renewalDate != nil && strings.TrimSpace(*renewalDate) != "" {
		t, err := time.Parse("2006-01-02", strings.TrimSpace(*renewalDate))
		if err != nil {
			return nil, nil, nil, "plan_renewal_date deve estar no formato YYYY-MM-DD"
		}
		renewal = &t
	}
	var quota *int64
	if quotaTokens != nil {
		if *quotaTokens <= 0 {
			return nil, nil, nil, "plan_quota_tokens deve ser maior que zero"
		}
		quota = quotaTokens
	}
	var name *string
	if s := strings.TrimSpace(planName); s != "" {
		if len(s) > 100 {
			s = s[:100]
		}
		name = &s
	}
	return name, renewal, quota, ""
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

	// Campos de plano (issue #19): validação leve no payload.
	planName, planRenewal, planQuota, planErr := parsePlanFields(req.PlanName, req.PlanRenewalDate, req.PlanQuotaTokens)
	if planErr != "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", planErr)
		return
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
		PlanName:          planName,
		PlanRenewalDate:   planRenewal,
		PlanQuotaTokens:   planQuota,
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
	// Auditoria (issue #19, RF-03): transição manual com o ator admin.
	// Best-effort — falha de audit não reverte a transição.
	a.auditKeyTransition(r, sess, id, "provider_key.manual_disable",
		map[string]any{"status": "manual_disabled"})
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
	// Auditoria (issue #19, RF-03): reativação manual com o ator admin.
	// Best-effort — falha de audit não reverte a transição.
	a.auditKeyTransition(r, sess, id, "provider_key.manual_enable",
		map[string]any{"status": "enabled"})
	writeJSON(w, http.StatusOK, map[string]string{"status": "enabled"})
}

// auditKeyTransition registra um evento de transição de chave no audit_log
// com o ator admin da sessão (issue #19, RF-03). Best-effort: falhas são
// silenciosas — a auditoria não pode derrubar a operação já persistida.
func (a *App) auditKeyTransition(r *http.Request, sess *sessionContext, keyID int64, action string, metadata map[string]any) {
	if a.auditRepo == nil || sess == nil {
		return
	}
	actor := int64(sess.UserID)
	kid := keyID
	_ = a.auditRepo.Append(r.Context(), postgres.AuditEntry{
		OrganizationID: int64(sess.OrgID),
		ActorUserID:    &actor,
		Action:         action,
		EntityType:     "provider_key",
		EntityID:       &kid,
		Metadata:       metadata,
	})
}

// --- Admin: Providers CRUD extras -------------------------------------------

// updateProviderRequest payload de PATCH /api/web/admin/providers/{id}.
type updateProviderRequest struct {
	Name              string  `json:"name"`
	Slug              string  `json:"slug"`
	BaseURL           string  `json:"base_url"`
	APISpec           string  `json:"api_spec"`
	UserAgentOverride *string `json:"user_agent_override"`
}

func providerFromPath(r *http.Request) (int64, bool) {
	return pathID(r.URL.Path, "/api/web/admin/providers/")
}

// handleAdminGetProvider retorna um provider pelo ID.
func (a *App) handleAdminGetProvider(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := providerFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid provider id")
		return
	}
	p, err := a.providerRepo.GetByID(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id))
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not load provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": providerListItem{
			ID:                int64(p.ID),
			Name:              p.Name,
			Slug:              p.Slug,
			BaseURL:           p.BaseURL,
			APISpec:           p.APISpec,
			UserAgentOverride: p.UserAgentGlobal,
			IsActive:          p.IsActive,
			CreatedAt:         p.CreatedAt,
		},
	})
}

// handleAdminUpdateProvider atualiza um provider.
func (a *App) handleAdminUpdateProvider(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := providerFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid provider id")
		return
	}
	var req updateProviderRequest
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid body")
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
	if err := domain.ValidateProviderURL(req.BaseURL, domain.WithSSRFAllowInsecureHTTP(!a.cookieCfg.Secure)); err != nil {
		if domain.IsSSRFBlocked(err) {
			writeAPIError(w, http.StatusBadRequest, "ssrf_blocked", "base_url blocked by SSRF guard")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "dns_error", "could not resolve base_url host")
		return
	}
	p := &domain.Provider{
		ID:              domain.ID(id),
		OrgID:           domain.OrganizationID(sess.OrgID),
		Name:            req.Name,
		Slug:            req.Slug,
		BaseURL:         req.BaseURL,
		APISpec:         req.APISpec,
		UserAgentGlobal: req.UserAgentOverride,
	}
	if err := a.providerRepo.Update(r.Context(), domain.OrganizationID(sess.OrgID), p); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		if errors.Is(err, postgres.ErrConflict) {
			writeAPIError(w, http.StatusConflict, "slug_taken", "slug already exists")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not update provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleAdminToggleProviderActive ativa/inativa um provider.
func (a *App) handleAdminToggleProviderActive(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := providerFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid provider id")
		return
	}
	var req struct {
		Active bool `json:"active"`
	}
	if err := parseJSON(r, &req, 1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid body")
		return
	}
	if err := a.providerRepo.SetActive(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id), req.Active); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not update provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"active": req.Active})
}

// handleAdminDeleteProvider remove um provider fisicamente.
func (a *App) handleAdminDeleteProvider(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := providerFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid provider id")
		return
	}
	if err := a.providerRepo.Delete(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		if errors.Is(err, postgres.ErrConflict) {
			writeAPIError(w, http.StatusConflict, "has_dependents", "provider has models or keys")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not delete provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Admin: Keys CRUD extras -------------------------------------------------

// updateKeyRequest payload de PATCH /api/web/admin/keys/{id}.
type updateKeyRequest struct {
	Label             string  `json:"label"`
	UserAgentOverride *string `json:"user_agent_override"`
	// Campos de plano (issue #19). Ausentes/vazios → NULL (update completo
	// do plano: enviar vazio limpa o campo).
	PlanName        string  `json:"plan_name"`
	PlanRenewalDate *string `json:"plan_renewal_date,omitempty"` // YYYY-MM-DD
	PlanQuotaTokens *int64  `json:"plan_quota_tokens,omitempty"`
}

func keyFromPath(r *http.Request) (int64, bool) {
	return pathID(r.URL.Path, "/api/web/admin/keys/")
}

// handleAdminGetKey retorna uma chave pelo ID.
func (a *App) handleAdminGetKey(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := keyFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid key id")
		return
	}
	k, err := a.keyRepo.GetByID(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id))
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "key not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not load key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key": keyListItemFromDomain(k),
	})
}

// keyListItemFromDomain monta o DTO de listagem a partir da entidade
// (issue #19: inclui os campos de plano; a data é formatada YYYY-MM-DD).
func keyListItemFromDomain(k *domain.ProviderKey) keyListItem {
	item := keyListItem{
		ID:                int64(k.ID),
		ProviderID:        int64(k.ProviderID),
		Label:             k.Label,
		Status:            string(k.Status),
		DisabledReason:    (*string)(k.DisabledReason),
		DisabledUntil:     k.DisabledUntil,
		ErrorCount:        k.ErrorCount,
		LastUsedAt:        k.LastUsedAt,
		UserAgentOverride: k.UserAgentOverride,
		PlanName:          k.PlanName,
		PlanQuotaTokens:   k.PlanQuotaTokens,
		CreatedAt:         k.CreatedAt,
	}
	if k.PlanRenewalDate != nil {
		s := k.PlanRenewalDate.UTC().Format("2006-01-02")
		item.PlanRenewalDate = &s
	}
	return item
}

// handleAdminUpdateKey atualiza apenas os metadados de uma chave.
func (a *App) handleAdminUpdateKey(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := keyFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid key id")
		return
	}
	var req updateKeyRequest
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid body")
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "label is required")
		return
	}
	if len(label) > 100 {
		label = label[:100]
	}
	planName, planRenewal, planQuota, planErr := parsePlanFields(req.PlanName, req.PlanRenewalDate, req.PlanQuotaTokens)
	if planErr != "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", planErr)
		return
	}
	if err := a.keyRepo.UpdateMetadata(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id),
		label, req.UserAgentOverride, planName, planRenewal, planQuota); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "key not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not update key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "label": label})
}

// handleAdminDeleteKey remove uma chave fisicamente.
func (a *App) handleAdminDeleteKey(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := keyFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid key id")
		return
	}
	if err := a.keyRepo.Delete(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "key not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not delete key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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
