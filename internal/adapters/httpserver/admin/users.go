package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Handlers admin de gestão de usuários (/api/web/admin/users).
//
// Acesso exclusivo de role=admin (garantido por withAdminAuth no App.Handler).
//
// Operações:
//   - GET    /api/web/admin/users          → lista todos (sem password_hash).
//   - POST   /api/web/admin/users          → cria novo (admin atribui role).
//   - PATCH  /api/web/admin/users/{id}/status → habilita/desabilita (revogação
//     imediata via DB hit no proxy — spec §3.3.3).
//
// Segurança:
//   - password_hash JAMAIS exposto em listagens (uso de userPublic).
//   - Mass assignment prevenido: structs DTO explícitas (createUserRequest,
//     setUserStatusRequest) — nunca bind direto para domain.User.
//   - Admin não pode desabilitar a si mesmo (lockout) — validação explícita.
// =============================================================================

// userAdminListItem para listagem admin (sem password_hash).
type userAdminListItem struct {
	ID         int64      `json:"id"`
	Username   string     `json:"username"`
	Role       string     `json:"role"`
	Status     string     `json:"status"`
	OrgID      int64      `json:"org_id"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	LastLogin  *time.Time `json:"last_login_at,omitempty"`
}

// createUserRequest payload de POST /api/web/admin/users.
type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"` // "admin" | "user" (default: user)
}

// setUserStatusRequest payload de PATCH /api/web/admin/users/{id}/status.
type setUserStatusRequest struct {
	Status string `json:"status"` // "active" | "disabled"
}

// handleAdminListUsers retorna todos os usuários da org.
//
// TODO F8: adicionar paginação + filtros (role, status). Para ~30 usuários
// internos, retorno simples é suficiente.
func (a *App) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	// Reutilizamos UserRepo via query direta — para listagem, fazemos SELECT
	// direto (não há método ListAll ainda no repo). Para F4 simples, iteramos
	// via uma query mínima usando o pool.
	rows, err := a.pool.Query(r.Context(), `
		SELECT id, organization_id, username, role, status, disabled_at, created_at, updated_at, last_login_at
		FROM users
		WHERE organization_id = $1
		ORDER BY username`, sess.OrgID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not list users")
		return
	}
	defer rows.Close()

	out := make([]userAdminListItem, 0)
	for rows.Next() {
		var u userAdminListItem
		var lastLogin *time.Time
		if err := rows.Scan(&u.ID, &u.OrgID, &u.Username, &u.Role, &u.Status, &u.DisabledAt, &u.CreatedAt, &u.UpdatedAt, &lastLogin); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal", "scan error")
			return
		}
		u.LastLogin = lastLogin
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// handleAdminCreateUser cria um novo usuário.
//
// Validações:
//   - username não vazio, 3-50 chars alnum/underscore.
//   - senha mínimo 12 chars (NIST 800-63B — sem exigências pedantes de chars).
//   - role deve ser admin|user (default: user).
//
// Em erro de UNIQUE (username duplicado), retorna 409 (não revelar se usuário
// existe — mas admin precisa saber para corrigir; aqui admin é confiável).
func (a *App) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}

	var req createUserRequest
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	username := strings.TrimSpace(req.Username)
	if len(username) < 3 || len(username) > 50 {
		writeAPIError(w, http.StatusBadRequest, "invalid_username", "username must be 3-50 chars")
		return
	}
	if !isSafeUsername(username) {
		writeAPIError(w, http.StatusBadRequest, "invalid_username", "username must contain only letters, digits, underscore, hyphen")
		return
	}
	if len(req.Password) < 12 {
		writeAPIError(w, http.StatusBadRequest, "weak_password", "password must be at least 12 characters")
		return
	}
	role := domain.Role(req.Role)
	if role != domain.RoleAdmin && role != domain.RoleUser {
		role = domain.RoleUser
	}

	hash, err := domain.HashPassword([]byte(req.Password))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not hash password")
		return
	}

	user := &domain.User{
		OrgID:        domain.OrganizationID(sess.OrgID),
		Username:     username,
		PasswordHash: []byte(hash),
		Role:         role,
		Status:       domain.UserStatusActive,
	}
	id, err := a.userRepo.Create(r.Context(), user)
	if err != nil {
		// Detecção de UNIQUE violation (username duplicado) fica a cargo do
		// caller via errors.As para *pgconn.PgError com code 23505.
		// Não expomos detalhes técnicos; admin vê 409 com mensagem útil.
		if isUniqueViolation(err) {
			writeAPIError(w, http.StatusConflict, "username_taken", "username already exists")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not create user")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":       int64(id),
		"username": username,
		"role":     string(role),
		"status":   string(domain.UserStatusActive),
	})
}

// updateUserRequest payload de PATCH /api/web/admin/users/{id}.
type updateUserRequest struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	Status   string `json:"status"`
}

func userFromPath(r *http.Request) (int64, bool) {
	return pathID(r.URL.Path, "/api/web/admin/users/")
}

// handleAdminGetUser retorna um usuário pelo ID.
func (a *App) handleAdminGetUser(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := userFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	u, err := a.userRepo.GetByID(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id))
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not load user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": userAdminListItem{
			ID:         int64(u.ID),
			Username:   u.Username,
			Role:       string(u.Role),
			Status:     string(u.Status),
			OrgID:      int64(u.OrgID),
			CreatedAt:  u.CreatedAt,
			UpdatedAt:  u.UpdatedAt,
			DisabledAt: u.DisabledAt,
		},
	})
}

// handleAdminUpdateUser atualiza username, role e status de um usuário.
func (a *App) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := userFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	var req updateUserRequest
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	username := strings.TrimSpace(req.Username)
	if len(username) < 3 || len(username) > 50 || !isSafeUsername(username) {
		writeAPIError(w, http.StatusBadRequest, "invalid_username", "username must be 3-50 chars, alphanumeric/underscore/hyphen")
		return
	}
	role := domain.Role(req.Role)
	if role != domain.RoleAdmin && role != domain.RoleUser {
		writeAPIError(w, http.StatusBadRequest, "invalid_role", "role must be admin or user")
		return
	}
	status := domain.UserStatus(req.Status)
	if status != domain.UserStatusActive && status != domain.UserStatusDisabled {
		writeAPIError(w, http.StatusBadRequest, "invalid_status", "status must be active or disabled")
		return
	}
	var disabledAt *time.Time
	if status == domain.UserStatusDisabled {
		now := time.Now()
		disabledAt = &now
	}
	u := &domain.User{
		ID:         domain.ID(id),
		OrgID:      domain.OrganizationID(sess.OrgID),
		Username:   username,
		Role:       role,
		Status:     status,
		DisabledAt: disabledAt,
	}
	if err := a.userRepo.Update(r.Context(), domain.OrganizationID(sess.OrgID), u); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		if errors.Is(err, postgres.ErrConflict) {
			writeAPIError(w, http.StatusConflict, "username_taken", "username already exists")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not update user")
		return
	}
	if status == domain.UserStatusDisabled {
		_ = a.sessions.DeleteAllForUser(r.Context(), domain.ID(id))
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": string(status)})
}

// handleAdminDeleteUser remove um usuário fisicamente. Tokens são deletados em
// cascata pelo DB (FK). Admin não pode se deletar.
func (a *App) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	id, ok := userFromPath(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	if id == sess.UserID {
		writeAPIError(w, http.StatusConflict, "self_action_forbidden", "cannot delete yourself")
		return
	}
	if err := a.userRepo.Delete(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id)); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not delete user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleAdminSetUserStatus habilita/desabilita usuário.
//
// Importante (spec §3.3.3): ao desabilitar, TODAS as sessões web do usuário
// são invalidadas (DeleteAllForUser) E o proxy nega próximas requisições
// (DB hit por request valida user.Status no TokenValidator). Revogação
// imediata e determinística.
//
// Bloqueio de auto-desabilitação: admin não pode desabilitar a si mesmo (evita
// lockout acidental onde o único admin se tranca fora).
func (a *App) handleAdminSetUserStatus(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}

	// Extrai {id} do path /api/web/admin/users/{id}/status
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/web/admin/users/"), "/")
	if len(parts) < 1 {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid path")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}

	// Anti-lockout: admin não pode desabilitar a si.
	if id == sess.UserID {
		writeAPIError(w, http.StatusConflict, "self_action_forbidden", "cannot change own status")
		return
	}

	var req setUserStatusRequest
	if err := parseJSON(r, &req, 1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	var newStatus domain.UserStatus
	var disabledAt *time.Time
	switch domain.UserStatus(req.Status) {
	case domain.UserStatusActive:
		newStatus = domain.UserStatusActive
		disabledAt = nil
	case domain.UserStatusDisabled:
		newStatus = domain.UserStatusDisabled
		now := time.Now()
		disabledAt = &now
	default:
		writeAPIError(w, http.StatusBadRequest, "invalid_status", "status must be active or disabled")
		return
	}

	if err := a.userRepo.SetStatus(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id), newStatus, disabledAt); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not update status")
		return
	}

	// Revogação imediata de sessões web se desabilitado.
	if newStatus == domain.UserStatusDisabled {
		_ = a.sessions.DeleteAllForUser(r.Context(), domain.ID(id))
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": string(newStatus)})
}

// isSafeUsername valida alfabeto de username: [A-Za-z0-9_-].
// Previne injeção em logs/UIs e caracteres ambíguos (homoglyphs).
func isSafeUsername(s string) bool {
	for _, c := range s {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// isUniqueViolation detecta erro de UNIQUE constraint do PostgreSQL (code 23505).
// Implementação leve (não importa pgconn para evitar acoplamento aqui).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// String-match no erro — aceitável para F4 (sem importar pgconn). Em F8,
	// podemos refinar com errors.As para *pgconn.PgError.
	return strings.Contains(err.Error(), "23505") || strings.Contains(err.Error(), "unique constraint")
}
