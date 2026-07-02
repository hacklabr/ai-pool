package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserRepo persiste e consulta usuários (spec §3.4.1 item 2).
//
// Métodos essenciais para F4 (auth/RBAC) e F8 (admin UI). Métodos de quota/
// auditoria ficam como stubs comentados para fases futuras.
type UserRepo struct {
	pool *pgxpool.Pool
}

// NewUserRepo cria o repositório. O pool é compartilhado (singleton da app).
func NewUserRepo(pool *pgxpool.Pool) *UserRepo { return &UserRepo{pool: pool} }

// Create insere um novo usuário. password_hash deve vir já hasheado (Argon2id)
// — o repositório NUNCA hashea (responsabilidade do serviço de domínio/auth).
// Retorna o ID gerado.
func (r *UserRepo) Create(ctx context.Context, u *domain.User) (domain.ID, error) {
	if u == nil {
		return 0, errors.New("UserRepo.Create: nil user")
	}
	const q = `
		INSERT INTO users (organization_id, username, password_hash, password_algo, role, status)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, 'active'))
		RETURNING id`
	var id int64
	err := r.pool.QueryRow(ctx, q,
		int64(u.OrgID),
		u.Username,
		string(u.PasswordHash),
		"argon2id",
		string(u.Role),
		string(u.Status),
	).Scan(&id)
	if err != nil {
		// Detecção de UNIQUE violation (username duplicado) fica a cargo do
		// caller via errors.As para *pgconn.PgError com code 23505.
		return 0, fmt.Errorf("UserRepo.Create: %w", err)
	}
	return domain.ID(id), nil
}

// GetByID carrega um usuário pela PK. Retorna ErrNotFound se inexistente.
func (r *UserRepo) GetByID(ctx context.Context, orgID domain.OrganizationID, id domain.ID) (*domain.User, error) {
	const q = `
		SELECT id, organization_id, username, password_hash, role, status, disabled_at, created_at, updated_at
		FROM users
		WHERE organization_id = $1 AND id = $2`
	row := r.pool.QueryRow(ctx, q, int64(orgID), int64(id))
	u, err := scanUser(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("UserRepo.GetByID: %w", err)
	}
	return u, nil
}

// GetByUsername carrega um usuário pelo username (para login). Índice UNIQUE
// (organization_id, username) torna isto O(1).
func (r *UserRepo) GetByUsername(ctx context.Context, orgID domain.OrganizationID, username string) (*domain.User, error) {
	const q = `
		SELECT id, organization_id, username, password_hash, role, status, disabled_at, created_at, updated_at
		FROM users
		WHERE organization_id = $1 AND username = $2`
	row := r.pool.QueryRow(ctx, q, int64(orgID), username)
	u, err := scanUser(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("UserRepo.GetByUsername: %w", err)
	}
	return u, nil
}

// SetStatus atualiza status (active/disabled) e disabled_at atomicamente.
// Usado para desabilitar/habilitar usuários (revogação imediata de acesso ao
// pool, spec §3.3.3). O middleware de auth valida status no DB hit por request.
func (r *UserRepo) SetStatus(ctx context.Context, orgID domain.OrganizationID, id domain.ID, status domain.UserStatus, disabledAt *time.Time) error {
	const q = `
		UPDATE users
		SET status = $3, disabled_at = $4, updated_at = now()
		WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id), string(status), ptrToTimestamptz(disabledAt))
	if err != nil {
		return fmt.Errorf("UserRepo.SetStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdatePassword troca o hash de senha. Usado em "reset password" (F4).
// password_hash deve vir já hasheado (Argon2id).
func (r *UserRepo) UpdatePassword(ctx context.Context, orgID domain.OrganizationID, id domain.ID, passwordHash []byte) error {
	const q = `
		UPDATE users SET password_hash = $3, updated_at = now()
		WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id), string(passwordHash))
	if err != nil {
		return fmt.Errorf("UserRepo.UpdatePassword: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAll retorna todos os usuários da organização (ativos e desabilitados).
func (r *UserRepo) ListAll(ctx context.Context, orgID domain.OrganizationID) ([]domain.User, error) {
	const q = `
		SELECT id, organization_id, username, password_hash, role, status, disabled_at, created_at, updated_at
		FROM users
		WHERE organization_id = $1
		ORDER BY username`
	rows, err := r.pool.Query(ctx, q, int64(orgID))
	if err != nil {
		return nil, fmt.Errorf("UserRepo.ListAll: %w", err)
	}
	defer rows.Close()

	var out []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("UserRepo.ListAll scan: %w", err)
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// Update altera username, role e status de um usuário. Não altera senha.
func (r *UserRepo) Update(ctx context.Context, orgID domain.OrganizationID, u *domain.User) error {
	if u == nil {
		return errors.New("UserRepo.Update: nil user")
	}
	const q = `
		UPDATE users
		SET username = $3, role = $4, status = $5, disabled_at = $6, updated_at = now()
		WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q,
		int64(orgID), int64(u.ID), u.Username, string(u.Role), string(u.Status), ptrToTimestamptz(u.DisabledAt),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("UserRepo.Update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete remove um usuário fisicamente, deletando seus tokens em cascata.
// O chamador deve garantir que não há tokens ativos ou decidir sobre eles.
func (r *UserRepo) Delete(ctx context.Context, orgID domain.OrganizationID, id domain.ID) error {
	// FK física em api_tokens.user_id → users(id); removemos tokens primeiro.
	if _, err := r.pool.Exec(ctx, `DELETE FROM api_tokens WHERE organization_id = $1 AND user_id = $2`, int64(orgID), int64(id)); err != nil {
		return fmt.Errorf("UserRepo.Delete: tokens: %w", err)
	}
	const q = `DELETE FROM users WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id))
	if err != nil {
		return fmt.Errorf("UserRepo.Delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// scanUser é compartilhado por GetByID/GetByUsername. Centraliza o mapeamento
// coluna→struct (DRY). row implementa a interface mínima pgx.Row.
func scanUser(row pgx.Row) (*domain.User, error) {
	var (
		u           domain.User
		orgID       int64
		pwdHash     string
		role        string
		status      string
		disabledAtT pgtype.Timestamptz
	)
	err := row.Scan(
		&u.ID, &orgID, &u.Username, &pwdHash, &role, &status, &disabledAtT, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	u.OrgID = domain.OrganizationID(orgID)
	u.PasswordHash = []byte(pwdHash)
	u.Role = domain.Role(role)
	u.Status = domain.UserStatus(status)
	u.DisabledAt = timestamptzToPtr(disabledAtT)
	return &u, nil
}

// =============================================================================
// Stubs para fases futuras (mantidos como referência, NÃO implementados no MVP):
//
//   - ListByOrg(orgID, filter, page)  -> F8 admin UI (paginação).
//   - TouchFailedLogin(id, increment) -> F4 brute-force protection.
//   - SoftDelete(id)                  -> RGPD "direito ao esquecimento" (roadmap).
//   - CountActive()                   -> métrica /metrics (F6).
// =============================================================================
