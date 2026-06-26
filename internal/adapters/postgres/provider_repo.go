package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rafael/ai-pool/internal/domain"
)

// ProviderRepo persiste provedores de LLM (spec §3.4.1 item 4).
type ProviderRepo struct {
	pool *pgxpool.Pool
}

// NewProviderRepo cria o repositório.
func NewProviderRepo(pool *pgxpool.Pool) *ProviderRepo { return &ProviderRepo{pool: pool} }

// Create insere um novo provedor. base_url deve passar pelo SSRF guard na
// camada de aplicação ANTES de chegar aqui (spec §3.3.6).
func (r *ProviderRepo) Create(ctx context.Context, p *domain.Provider) (domain.ID, error) {
	if p == nil {
		return 0, errors.New("ProviderRepo.Create: nil provider")
	}
	const q = `
		INSERT INTO providers (organization_id, name, slug, base_url, api_spec,
		                       default_user_agent_override, is_active)
		VALUES ($1, $2, $3, $4, COALESCE($5,'openai'), $6, COALESCE($7, true))
		RETURNING id`
	var id int64
	err := r.pool.QueryRow(ctx, q,
		int64(p.OrgID), p.Name, p.Slug, p.BaseURL, p.APISpec,
		ptrToText(p.UserAgentGlobal), p.IsActive,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ProviderRepo.Create: %w", err)
	}
	return domain.ID(id), nil
}

// GetByID carrega um provedor pela PK.
func (r *ProviderRepo) GetByID(ctx context.Context, orgID domain.OrganizationID, id domain.ID) (*domain.Provider, error) {
	const q = `
		SELECT id, organization_id, name, slug, base_url, api_spec,
		       default_user_agent_override, is_active, created_at, updated_at
		FROM providers
		WHERE organization_id = $1 AND id = $2`
	row := r.pool.QueryRow(ctx, q, int64(orgID), int64(id))
	p, err := scanProvider(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("ProviderRepo.GetByID: %w", err)
	}
	return p, nil
}

// GetBySlug carrega um provedor pelo slug (UNIQUE por org).
func (r *ProviderRepo) GetBySlug(ctx context.Context, orgID domain.OrganizationID, slug string) (*domain.Provider, error) {
	const q = `
		SELECT id, organization_id, name, slug, base_url, api_spec,
		       default_user_agent_override, is_active, created_at, updated_at
		FROM providers
		WHERE organization_id = $1 AND slug = $2`
	row := r.pool.QueryRow(ctx, q, int64(orgID), slug)
	p, err := scanProvider(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("ProviderRepo.GetBySlug: %w", err)
	}
	return p, nil
}

// ListActive retorna todos os provedores ativos da organização.
// Usado no boot para popular caches e no admin UI.
func (r *ProviderRepo) ListActive(ctx context.Context, orgID domain.OrganizationID) ([]domain.Provider, error) {
	const q = `
		SELECT id, organization_id, name, slug, base_url, api_spec,
		       default_user_agent_override, is_active, created_at, updated_at
		FROM providers
		WHERE organization_id = $1 AND is_active = true
		ORDER BY name`
	rows, err := r.pool.Query(ctx, q, int64(orgID))
	if err != nil {
		return nil, fmt.Errorf("ProviderRepo.ListActive: %w", err)
	}
	defer rows.Close()

	var out []domain.Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("ProviderRepo.ListActive scan: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// scanProvider aceita pgx.Row (single) ou pgx.Rows (iterator). Ambos têm Scan().
func scanProvider(row pgx.Row) (*domain.Provider, error) {
	var (
		p        domain.Provider
		orgID    int64
		uaGlobal pgtype.Text
	)
	err := row.Scan(
		&p.ID, &orgID, &p.Name, &p.Slug, &p.BaseURL, &p.APISpec,
		&uaGlobal, &p.IsActive, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	p.OrgID = domain.OrganizationID(orgID)
	p.UserAgentGlobal = textToPtr(uaGlobal)
	return &p, nil
}
