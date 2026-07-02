package admin

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Handlers admin de gestão de tokens de API (/api/web/admin/tokens).
//
// Admin pode listar, revogar e deletar tokens de qualquer usuário da org.
// A criação de token continua sendo responsabilidade do próprio usuário via
// /api/web/me/tokens (secret só é exibido uma vez no momento da geração).
// =============================================================================

// adminTokenListItem para listagem admin.
type adminTokenListItem struct {
	ID         int64   `json:"id"`
	UserID     int64   `json:"user_id"`
	Username   string  `json:"username"`
	KeyID      string  `json:"key_id"`
	MaskedFull string  `json:"masked_token"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  *string `json:"expires_at,omitempty"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
}

// handleAdminListTokens lista todos os tokens da org, opcionalmente filtrando
// por user_id via query string ?user_id=.
func (a *App) handleAdminListTokens(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	var userID *domain.ID
	if q := r.URL.Query().Get("user_id"); q != "" {
		if n, err := strconv.ParseInt(q, 10, 64); err == nil && n > 0 {
			uid := domain.ID(n)
			userID = &uid
		}
	}
	items, err := a.tokenRepo.ListAll(r.Context(), domain.OrganizationID(sess.OrgID), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not list tokens")
		return
	}

	// Resolve usernames para exibição.
	usernames := make(map[domain.ID]string)
	for _, it := range items {
		if _, ok := usernames[it.UserID]; !ok {
			if u, err := a.userRepo.GetByID(r.Context(), domain.OrganizationID(sess.OrgID), it.UserID); err == nil {
				usernames[it.UserID] = u.Username
			} else {
				usernames[it.UserID] = "?"
			}
		}
	}

	out := make([]adminTokenListItem, 0, len(items))
	for _, it := range items {
		out = append(out, adminTokenListItem{
			ID:         int64(it.ID),
			UserID:     int64(it.UserID),
			Username:   usernames[it.UserID],
			KeyID:      it.KeyID,
			MaskedFull: domain.MaskedToken(it.KeyID),
			Name:       it.Name,
			Status:     string(it.Status),
			CreatedAt:  it.CreatedAt.Format("2006-01-02T15:04:05Z"),
			ExpiresAt:  formatOptionalTime(it.ExpiresAt),
			LastUsedAt: formatOptionalTime(it.LastUsedAt),
			RevokedAt:  formatOptionalTime(it.RevokedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// handleAdminRevokeToken revoga um token de qualquer usuário da org.
func (a *App) handleAdminRevokeToken(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := pathID(r.URL.Path, "/api/web/admin/tokens/")
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid token id")
		return
	}
	if err := a.tokenRepo.Revoke(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "token not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not revoke token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleAdminDeleteToken remove um token fisicamente.
func (a *App) handleAdminDeleteToken(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := pathID(r.URL.Path, "/api/web/admin/tokens/")
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid token id")
		return
	}
	if err := a.tokenRepo.Delete(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "token not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not delete token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func formatOptionalTime(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := t.Format("2006-01-02T15:04:05Z")
	return &s
}
