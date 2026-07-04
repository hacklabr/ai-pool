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

// InvitationRepo persiste e consulta tokens de convite de usuários.
type InvitationRepo struct {
	pool *pgxpool.Pool
}

// NewInvitationRepo cria o repositório.
func NewInvitationRepo(pool *pgxpool.Pool) *InvitationRepo { return &InvitationRepo{pool: pool} }

// Create insere um novo convite. tokenHash deve ser o SHA-256 do token raw.
func (r *InvitationRepo) Create(ctx context.Context, t *domain.InvitationToken) (domain.ID, error) {
	if t == nil {
		return 0, errors.New("InvitationRepo.Create: nil invitation")
	}
	const q = `
		INSERT INTO invitation_tokens
		    (organization_id, token_hash, token_prefix, username, role, status,
		     created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, 'pending'), $7, $8)
		RETURNING id`
	var id int64
	err := r.pool.QueryRow(ctx, q,
		int64(t.OrgID),
		t.TokenHash,
		t.TokenPrefix,
		t.Username,
		string(t.Role),
		string(t.Status),
		int64(t.CreatedBy),
		t.ExpiresAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("InvitationRepo.Create: %w", err)
	}
	return domain.ID(id), nil
}

// GetPendingByHashForUpdate carrega um convite pending pelo hash, com lock
// exclusivo para prevenir race conditions no aceite.
func (r *InvitationRepo) GetPendingByHashForUpdate(ctx context.Context, orgID domain.OrganizationID, tokenHash string) (*domain.InvitationToken, error) {
	const q = `
		SELECT id, organization_id, token_hash, token_prefix, username, role, status,
		       created_by, used_by, used_at, expires_at, created_at, updated_at
		FROM invitation_tokens
		WHERE organization_id = $1 AND token_hash = $2 AND status = 'pending'
		FOR UPDATE`
	row := r.pool.QueryRow(ctx, q, int64(orgID), tokenHash)
	t, err := scanInvitationToken(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("InvitationRepo.GetPendingByHashForUpdate: %w", err)
	}
	return t, nil
}

// GetByID carrega um convite pela PK.
func (r *InvitationRepo) GetByID(ctx context.Context, orgID domain.OrganizationID, id domain.ID) (*domain.InvitationToken, error) {
	const q = `
		SELECT id, organization_id, token_hash, token_prefix, username, role, status,
		       created_by, used_by, used_at, expires_at, created_at, updated_at
		FROM invitation_tokens
		WHERE organization_id = $1 AND id = $2`
	row := r.pool.QueryRow(ctx, q, int64(orgID), int64(id))
	t, err := scanInvitationToken(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("InvitationRepo.GetByID: %w", err)
	}
	return t, nil
}

// MarkUsed atomically marca o convite como usado e associa ao novo usuário.
func (r *InvitationRepo) MarkUsed(ctx context.Context, orgID domain.OrganizationID, id, usedBy domain.ID) error {
	const q = `
		UPDATE invitation_tokens
		SET status = 'used', used_by = $3, used_at = now(), updated_at = now()
		WHERE organization_id = $1 AND id = $2 AND status = 'pending'`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id), int64(usedBy))
	if err != nil {
		return fmt.Errorf("InvitationRepo.MarkUsed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// Revoke marca um convite pending como revogado.
func (r *InvitationRepo) Revoke(ctx context.Context, orgID domain.OrganizationID, id domain.ID) error {
	const q = `
		UPDATE invitation_tokens
		SET status = 'revoked', updated_at = now()
		WHERE organization_id = $1 AND id = $2 AND status = 'pending'`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id))
	if err != nil {
		return fmt.Errorf("InvitationRepo.Revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete remove um convite fisicamente.
func (r *InvitationRepo) Delete(ctx context.Context, orgID domain.OrganizationID, id domain.ID) error {
	const q = `DELETE FROM invitation_tokens WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id))
	if err != nil {
		return fmt.Errorf("InvitationRepo.Delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// InvitationListItem é a representação pública de um convite (sem hash).
type InvitationListItem struct {
	ID          domain.ID
	OrgID       domain.OrganizationID
	TokenPrefix string
	Username    string
	Role        domain.Role
	Status      domain.InvitationTokenStatus
	CreatedBy   domain.ID
	UsedBy      domain.ID
	UsedAt      *time.Time
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// ListByOrg retorna os convites de uma organização.
func (r *InvitationRepo) ListByOrg(ctx context.Context, orgID domain.OrganizationID) ([]InvitationListItem, error) {
	const q = `
		SELECT id, organization_id, token_prefix, username, role, status,
		       created_by, used_by, used_at, expires_at, created_at
		FROM invitation_tokens
		WHERE organization_id = $1
		ORDER BY created_at DESC`
	rows, err := r.pool.Query(ctx, q, int64(orgID))
	if err != nil {
		return nil, fmt.Errorf("InvitationRepo.ListByOrg: %w", err)
	}
	defer rows.Close()

	var items []InvitationListItem
	for rows.Next() {
		var it InvitationListItem
		var usedAtT pgtype.Timestamptz
		var usedBy, createdBy int64
		var status, role string
		err := rows.Scan(
			&it.ID, &it.OrgID, &it.TokenPrefix, &it.Username, &role, &status,
			&createdBy, &usedBy, &usedAtT, &it.ExpiresAt, &it.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("InvitationRepo.ListByOrg scan: %w", err)
		}
		it.Role = domain.Role(role)
		it.Status = domain.InvitationTokenStatus(status)
		it.CreatedBy = domain.ID(createdBy)
		it.UsedBy = domain.ID(usedBy)
		it.UsedAt = timestamptzToPtr(usedAtT)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("InvitationRepo.ListByOrg rows: %w", err)
	}
	return items, nil
}

// scanInvitationToken mapeia coluna→domain.InvitationToken.
func scanInvitationToken(row pgx.Row) (*domain.InvitationToken, error) {
	var (
		t         domain.InvitationToken
		orgID     int64
		createdBy int64
		usedBy    int64
		usedAtT   pgtype.Timestamptz
		status    string
		role      string
	)
	err := row.Scan(
		&t.ID, &orgID, &t.TokenHash, &t.TokenPrefix, &t.Username, &role, &status,
		&createdBy, &usedBy, &usedAtT, &t.ExpiresAt, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	t.OrgID = domain.OrganizationID(orgID)
	t.Role = domain.Role(role)
	t.Status = domain.InvitationTokenStatus(status)
	t.CreatedBy = domain.ID(createdBy)
	t.UsedBy = domain.ID(usedBy)
	t.UsedAt = timestamptzToPtr(usedAtT)
	return &t, nil
}

// InvitationAcceptResult contém os dados resultantes do aceite de um convite.
type InvitationAcceptResult struct {
	UserID      domain.ID
	Username    string
	OrgID       domain.OrganizationID
	APITokenRaw string
	TokenPrefix string
}

// Accept consome um convite pendente e cria o usuário + token API em uma única
// transação. Retorna ErrNotFound se o token não existe, não está pending ou
// expirou. Retorna ErrConflict se o convite já foi usado entre o SELECT e o
// UPDATE (race).
func (r *InvitationRepo) Accept(ctx context.Context, tokenHash string, password string, pepper []byte) (*InvitationAcceptResult, error) {
	if err := domain.ValidateInvitationPassword(password); err != nil {
		return nil, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("InvitationRepo.Accept: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now()

	// 1. Carrega convite pending com lock exclusivo.
	const getQ = `
		SELECT id, organization_id, token_hash, token_prefix, username, role, status,
		       created_by, used_by, used_at, expires_at, created_at, updated_at
		FROM invitation_tokens
		WHERE organization_id = $1 AND token_hash = $2 AND status = 'pending'
		FOR UPDATE`
	var (
		inv       domain.InvitationToken
		orgID     int64
		createdBy int64
		usedBy    int64
		usedAtT   pgtype.Timestamptz
		status    string
		role      string
	)
	err = tx.QueryRow(ctx, getQ, domain.DefaultOrganizationID, tokenHash).Scan(
		&inv.ID, &orgID, &inv.TokenHash, &inv.TokenPrefix, &inv.Username, &role, &status,
		&createdBy, &usedBy, &usedAtT, &inv.ExpiresAt, &inv.CreatedAt, &inv.UpdatedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("InvitationRepo.Accept: get invitation: %w", err)
	}
	inv.OrgID = domain.OrganizationID(orgID)
	inv.Role = domain.Role(role)
	inv.Status = domain.InvitationTokenStatus(status)
	inv.CreatedBy = domain.ID(createdBy)
	inv.UsedBy = domain.ID(usedBy)
	inv.UsedAt = timestamptzToPtr(usedAtT)

	if !inv.CanBeUsed(now) {
		return nil, ErrNotFound
	}

	// 2. Hash da senha.
	pwdHash, err := domain.HashPassword([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("InvitationRepo.Accept: hash password: %w", err)
	}

	// 3. Cria usuário.
	const userQ = `
		INSERT INTO users (organization_id, username, password_hash, password_algo, role, status)
		VALUES ($1, $2, $3, 'argon2id', $4, 'active')
		RETURNING id`
	var userID int64
	err = tx.QueryRow(ctx, userQ, int64(inv.OrgID), inv.Username, pwdHash, string(domain.RoleUser)).Scan(&userID)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("InvitationRepo.Accept: create user: %w", err)
	}

	// 4. Gera token API.
	rawToken, keyID, tokenHMAC, err := domain.GenerateToken(pepper)
	if err != nil {
		return nil, fmt.Errorf("InvitationRepo.Accept: generate API token: %w", err)
	}

	// #nosec G101 -- token_hmac é o nome da coluna no DB, não uma credencial.
	const tokenQ = `
		INSERT INTO api_tokens (user_id, organization_id, key_id, token_hmac, name, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, 'active', $6)`
	_, err = tx.Exec(ctx, tokenQ,
		userID, int64(inv.OrgID), keyID, tokenHMAC, "opencode", ptrToTimestamptz(nil),
	)
	if err != nil {
		return nil, fmt.Errorf("InvitationRepo.Accept: create API token: %w", err)
	}

	// 5. Marca convite como usado.
	const updateQ = `
		UPDATE invitation_tokens
		SET status = 'used', used_by = $3, used_at = now(), updated_at = now()
		WHERE organization_id = $1 AND id = $2 AND status = 'pending'`
	tag, err := tx.Exec(ctx, updateQ, int64(inv.OrgID), int64(inv.ID), userID)
	if err != nil {
		return nil, fmt.Errorf("InvitationRepo.Accept: mark used: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrConflict
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("InvitationRepo.Accept: commit: %w", err)
	}

	return &InvitationAcceptResult{
		UserID:      domain.ID(userID),
		Username:    inv.Username,
		OrgID:       inv.OrgID,
		APITokenRaw: rawToken,
		TokenPrefix: inv.TokenPrefix,
	}, nil
}
