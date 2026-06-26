package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rafael/ai-pool/internal/adapters/postgres"
	"github.com/rafael/ai-pool/internal/domain"
)

// =============================================================================
// Handlers de tokens do próprio usuário (/api/web/me/tokens).
//
// Qualquer role autenticada pode:
//   - Listar seus tokens (mascarados — sem secret).
//   - Criar novo token (retorna valor completo UMA ÚNICA vez).
//   - Revogar token (soft delete: seta revoked_at).
//
// Segurança:
//   - List/Create sempre com escopo userID = sessão atual (sem IDOR).
//   - Delete valida que o token pertence ao usuário ANTES de revogar.
//   - Secret do token JAMAIS é logado ou persistido (só HMAC).
// =============================================================================

// handleGetMe retorna os dados do usuário autenticado (auto-Introspection).
func (a *App) handleGetMe(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": userPublic{
			ID:       sess.UserID,
			Username: sess.Username,
			Role:     sess.Role,
			OrgID:    sess.OrgID,
		},
	})
}

// tokenListItemPublic é a representação pública de um token (sem secret/HMAC).
type tokenListItemPublic struct {
	ID         int64      `json:"id"`
	KeyID      string     `json:"key_id"`       // parte pública, ex.: "AB12CD34"
	MaskedFull string     `json:"masked_token"` // sk-aipp-AB12CD34-••••••••
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// handleListMyTokens retorna todos os tokens do usuário (incluindo revogados).
// Sem IDOR: sempre filtra por userID = sessão.
func (a *App) handleListMyTokens(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	items, err := a.tokenRepo.ListByUser(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(sess.UserID))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not list tokens")
		return
	}
	out := make([]tokenListItemPublic, 0, len(items))
	for _, it := range items {
		out = append(out, tokenListItemPublic{
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
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// createTokenRequest payload de POST /api/web/me/tokens.
type createTokenRequest struct {
	Name string `json:"name"` // rótulo opcional
	// ExpiresAt omitido por enquanto — expiração fica para refinamento futuro
	// (poderíamos adicionar campo opcional aqui).
}

// createTokenResponse retorna o valor COMPLETO do token uma única vez.
// O cliente DEVE copiar e armazenar com segurança — não há recuperação.
type createTokenResponse struct {
	Token     string              `json:"token"`  // sk-aipp-... (ÚNICA vez)
	KeyID     string              `json:"key_id"` // parte pública
	Name      string              `json:"name"`
	CreatedAt time.Time           `json:"created_at"`
	Public    tokenListItemPublic `json:"public"` // metadados públicos
}

// handleCreateMyToken gera um novo token sk-aipp-... e persiste apenas o HMAC.
//
// Fluxo:
//  1. domain.GenerateToken(pepper) → raw + keyID + tokenHMAC.
//  2. tokenRepo.Create com UserID=ctx + KeyID + TokenHMAC.
//  3. Retorna o valor RAW ao cliente (ÚNICA vez — não é recuperável).
//
// Importante: o secret JAMAIS é persistido. O DB guarda apenas o HMAC
// (HMAC-SHA256(secret, pepper)). Para validar um token enviado pelo cliente,
// recalculamos o HMAC e comparamos (constant-time) — ver TokenValidator.
func (a *App) handleCreateMyToken(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}

	var req createTokenRequest
	// Body opcional — se vazio, defaults aplicados.
	if r.ContentLength > 0 {
		if err := parseJSON(r, &req, 4*1024); err != nil {
			writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
	}
	// Sanitiza nome (limita tamanho, evita nomes absurdos).
	name := strings.TrimSpace(req.Name)
	if len(name) > 100 {
		name = name[:100]
	}

	// Gera token criptograficamente seguro.
	raw, keyID, tokenHMAC, err := domain.GenerateToken(a.pepper)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not generate token")
		return
	}

	now := time.Now()
	tok := &domain.APIToken{
		UserID:    domain.ID(sess.UserID),
		OrgID:     domain.OrganizationID(sess.OrgID),
		KeyID:     keyID,
		TokenHMAC: tokenHMAC,
		Name:      name,
		Status:    domain.APITokenStatusActive,
		CreatedAt: now,
	}
	id, err := a.tokenRepo.Create(r.Context(), tok)
	if err != nil {
		// Não devolver detalhes do erro do DB (pode vazar schema).
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not persist token")
		return
	}

	writeJSON(w, http.StatusCreated, createTokenResponse{
		Token:     raw,
		KeyID:     keyID,
		Name:      name,
		CreatedAt: now,
		Public: tokenListItemPublic{
			ID:         int64(id),
			KeyID:      keyID,
			MaskedFull: domain.MaskedToken(keyID),
			Name:       name,
			Status:     string(domain.APITokenStatusActive),
			CreatedAt:  now,
		},
	})
}

// handleRevokeMyToken revoga um token do próprio usuário.
// Soft delete (seta revoked_at). Irreversível.
//
// Anti-IDOR: valida que o token pertence ao usuário da sessão ANTES de revogar.
func (a *App) handleRevokeMyToken(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}

	// Extrai ID do path: /api/web/me/tokens/{id}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/web/me/tokens/")
	idStr = strings.TrimPrefix(idStr, "/")
	if idx := strings.Index(idStr, "/"); idx >= 0 {
		idStr = idStr[:idx]
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid token id")
		return
	}

	// Anti-IDOR: carrega o token e valida ownership.
	tok, err := a.tokenRepo.GetByID(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id))
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "token not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not load token")
		return
	}
	if tok.UserID != domain.ID(sess.UserID) {
		// Tentativa de revogar token alheio: responde 404 (não revelar existência).
		writeAPIError(w, http.StatusNotFound, "not_found", "token not found")
		return
	}
	if tok.RevokedAt != nil || tok.Status == domain.APITokenStatusRevoked {
		writeAPIError(w, http.StatusConflict, "already_revoked", "token already revoked")
		return
	}

	if err := a.tokenRepo.Revoke(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not revoke")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}
