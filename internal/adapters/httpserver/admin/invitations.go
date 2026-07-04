package admin

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Handlers admin de gestão de convites (/api/web/admin/invitations).
//
// Acesso exclusivo de role=admin (garantido por withAdminAuth no App.Handler).
//
// Operações:
//   - POST   /api/web/admin/invitations          → cria convite
//   - GET    /api/web/admin/invitations          → lista convites
//   - PATCH  /api/web/admin/invitations/{id}/revoke → revoga convite pending
//   - DELETE /api/web/admin/invitations/{id}     → remove convite
// =============================================================================

// createInvitationRequest payload de POST /api/web/admin/invitations.
type createInvitationRequest struct {
	Username string `json:"username"`
}

// invitationResponse é a representação pública de um convite.
type invitationResponse struct {
	ID           int64      `json:"id"`
	Username     string     `json:"username"`
	Role         string     `json:"role"`
	Status       string     `json:"status"`
	CreatedBy    int64      `json:"created_by"`
	UsedBy       *int64     `json:"used_by,omitempty"`
	UsedAt       *time.Time `json:"used_at,omitempty"`
	ExpiresAt    time.Time  `json:"expires_at"`
	CreatedAt    time.Time  `json:"created_at"`
	SetupCommand string     `json:"setup_command,omitempty"`
}

// handleAdminCreateInvitation cria um novo convite de usuário.
//
// O admin informa apenas o username. Role é sempre 'user'. O token raw é
// exibido uma única vez no setup_command.
func (a *App) handleAdminCreateInvitation(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}

	var req createInvitationRequest
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	username := strings.TrimSpace(req.Username)
	if !isSafeUsername(username) {
		writeAPIError(w, http.StatusBadRequest, "invalid_username", "username must be 3-50 chars, alphanumeric/underscore/hyphen")
		return
	}

	// Verifica se username já existe (evita convite para usuário existente).
	if _, err := a.userRepo.GetByUsername(r.Context(), domain.OrganizationID(sess.OrgID), username); err == nil {
		writeAPIError(w, http.StatusConflict, "username_taken", "username already exists")
		return
	} else if !errors.Is(err, postgres.ErrNotFound) {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not check username")
		return
	}

	raw, err := domain.GenerateInvitationToken()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not generate invitation token")
		return
	}

	now := time.Now()
	inv := &domain.InvitationToken{
		OrgID:       domain.OrganizationID(sess.OrgID),
		TokenHash:   raw.Hash,
		TokenPrefix: raw.Prefix,
		Username:    username,
		Role:        domain.RoleUser,
		Status:      domain.InvitationStatusPending,
		CreatedBy:   domain.ID(sess.UserID),
		ExpiresAt:   now.Add(domain.InvitationTokenExpiry),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	id, err := a.invitationRepo.Create(r.Context(), inv)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not create invitation")
		return
	}

	// Auditoria.
	_ = a.auditRepo.Append(r.Context(), postgres.AuditEntry{
		OrganizationID: sess.OrgID,
		ActorUserID:    &sess.UserID,
		Action:         "invitation.create",
		EntityType:     "invitation_token",
		EntityID:       ptrInt64(int64(id)),
		Metadata: map[string]any{
			"username":     username,
			"token_prefix": raw.Prefix,
		},
		IP:        net.ParseIP(clientIP(r)),
		UserAgent: r.UserAgent(),
	})

	setupURL := a.publicInviteURL(r) + "/invite/" + raw.Raw
	writeJSON(w, http.StatusCreated, invitationResponse{
		ID:           int64(id),
		Username:     username,
		Role:         string(domain.RoleUser),
		Status:       string(domain.InvitationStatusPending),
		CreatedBy:    sess.UserID,
		ExpiresAt:    inv.ExpiresAt,
		CreatedAt:    inv.CreatedAt,
		SetupCommand: fmt.Sprintf("curl -fsSL %s | bash", setupURL),
	})
}

// handleAdminListInvitations lista os convites da organização.
func (a *App) handleAdminListInvitations(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}

	items, err := a.invitationRepo.ListByOrg(r.Context(), domain.OrganizationID(sess.OrgID))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not list invitations")
		return
	}

	out := make([]invitationResponse, 0, len(items))
	for _, it := range items {
		out = append(out, invitationToResponse(it, ""))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitations": out})
}

// handleAdminRevokeInvitation revoga um convite pendente.
func (a *App) handleAdminRevokeInvitation(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := pathID(r.URL.Path, "/api/web/admin/invitations/")
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid invitation id")
		return
	}
	if err := a.invitationRepo.Revoke(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "invitation not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not revoke invitation")
		return
	}

	_ = a.auditRepo.Append(r.Context(), postgres.AuditEntry{
		OrganizationID: sess.OrgID,
		ActorUserID:    &sess.UserID,
		Action:         "invitation.revoke",
		EntityType:     "invitation_token",
		EntityID:       ptrInt64(id),
		IP:             net.ParseIP(clientIP(r)),
		UserAgent:      r.UserAgent(),
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleAdminDeleteInvitation remove um convite fisicamente.
func (a *App) handleAdminDeleteInvitation(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := pathID(r.URL.Path, "/api/web/admin/invitations/")
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid invitation id")
		return
	}
	if err := a.invitationRepo.Delete(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "invitation not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not delete invitation")
		return
	}

	_ = a.auditRepo.Append(r.Context(), postgres.AuditEntry{
		OrganizationID: sess.OrgID,
		ActorUserID:    &sess.UserID,
		Action:         "invitation.delete",
		EntityType:     "invitation_token",
		EntityID:       ptrInt64(id),
		IP:             net.ParseIP(clientIP(r)),
		UserAgent:      r.UserAgent(),
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// invitationToResponse converte InvitationListItem para response pública.
func invitationToResponse(it postgres.InvitationListItem, setupCommand string) invitationResponse {
	var usedBy *int64
	if it.UsedBy != 0 {
		v := int64(it.UsedBy)
		usedBy = &v
	}
	return invitationResponse{
		ID:           int64(it.ID),
		Username:     it.Username,
		Role:         string(it.Role),
		Status:       string(it.Status),
		CreatedBy:    int64(it.CreatedBy),
		UsedBy:       usedBy,
		UsedAt:       it.UsedAt,
		ExpiresAt:    it.ExpiresAt,
		CreatedAt:    it.CreatedAt,
		SetupCommand: setupCommand,
	}
}

// publicInviteURL retorna a URL base pública do serviço.
// Prioridade: configuração AIPP_PUBLIC_URL; fallback para esquema + Host do request.
func (a *App) publicInviteURL(r *http.Request) string {
	if a.publicURL != "" {
		return a.publicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = fwd
	}
	return scheme + "://" + r.Host
}

// ptrInt64 converte int64 para *int64.
func ptrInt64(v int64) *int64 {
	return &v
}

// acceptInvitationRequest payload de POST /api/invite/{token}/accept.
type acceptInvitationRequest struct {
	Password      string `json:"password"`
	SetupOpencode bool   `json:"setup_opencode"`
	SetupMimocode bool   `json:"setup_mimocode"`
}

// acceptInvitationResponse resposta de sucesso do aceite de convite.
type acceptInvitationResponse struct {
	User     userPublic     `json:"user"`
	APIToken string         `json:"api_token"`
	Models   map[string]any `json:"models"`
}
