package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
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

// ListAll retorna todos os provedores da organização (ativos e inativos).
func (r *ProviderRepo) ListAll(ctx context.Context, orgID domain.OrganizationID) ([]domain.Provider, error) {
	const q = `
		SELECT id, organization_id, name, slug, base_url, api_spec,
		       default_user_agent_override, is_active, created_at, updated_at
		FROM providers
		WHERE organization_id = $1
		ORDER BY name`
	rows, err := r.pool.Query(ctx, q, int64(orgID))
	if err != nil {
		return nil, fmt.Errorf("ProviderRepo.ListAll: %w", err)
	}
	defer rows.Close()

	var out []domain.Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("ProviderRepo.ListAll scan: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// Update altera nome, base_url, api_spec e user-agent override de um provedor.
func (r *ProviderRepo) Update(ctx context.Context, orgID domain.OrganizationID, p *domain.Provider) error {
	if p == nil {
		return errors.New("ProviderRepo.Update: nil provider")
	}
	const q = `
		UPDATE providers
		SET name = $3, slug = $4, base_url = $5, api_spec = $6,
		    default_user_agent_override = $7, updated_at = now()
		WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q,
		int64(orgID), int64(p.ID), p.Name, p.Slug, p.BaseURL, p.APISpec,
		ptrToText(p.UserAgentGlobal),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("ProviderRepo.Update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetActive ativa ou inativa um provedor.
func (r *ProviderRepo) SetActive(ctx context.Context, orgID domain.OrganizationID, id domain.ID, active bool) error {
	const q = `
		UPDATE providers
		SET is_active = $3, updated_at = now()
		WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id), active)
	if err != nil {
		return fmt.Errorf("ProviderRepo.SetActive: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete remove um provedor fisicamente. Retorna ErrConflict se ainda houver
// modelos ou chaves vinculados.
func (r *ProviderRepo) Delete(ctx context.Context, orgID domain.OrganizationID, id domain.ID) error {
	var dependents int
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT COUNT(*) FROM models WHERE provider_id = $1), 0) +
		       COALESCE((SELECT COUNT(*) FROM provider_keys WHERE provider_id = $1), 0)`,
		int64(id)).Scan(&dependents)
	if err != nil {
		return fmt.Errorf("ProviderRepo.Delete: %w", err)
	}
	if dependents > 0 {
		return ErrConflict
	}

	const q = `DELETE FROM providers WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id))
	if err != nil {
		return fmt.Errorf("ProviderRepo.Delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
