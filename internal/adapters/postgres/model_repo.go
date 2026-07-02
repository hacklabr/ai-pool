package postgres

import (
	"context"
	"fmt"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ModelRepo persiste e consulta modelos de LLM (spec §3.4.1 item 6).
//
// O hot path do proxy usa GetActiveProviderForModel: dado o nome do model
// solicitado pelo cliente (ex.: "deepseek-chat"), retorna o provider ativo que
// oferece este model. A resolução model→provider é o primeiro passo do routing.
type ModelRepo struct {
	pool *pgxpool.Pool
}

// NewModelRepo cria o repositório.
func NewModelRepo(pool *pgxpool.Pool) *ModelRepo { return &ModelRepo{pool: pool} }

// Create insere um novo modelo.
func (r *ModelRepo) Create(ctx context.Context, m *domain.Model) (domain.ID, error) {
	if m == nil {
		return 0, fmt.Errorf("ModelRepo.Create: nil model")
	}
	const q = `
		INSERT INTO models (provider_id, organization_id, name, display_name, context_window, is_active)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, true))
		RETURNING id`
	var id int64
	err := r.pool.QueryRow(ctx, q,
		int64(m.ProviderID), int64(m.OrgID), m.Name,
		ptrToText(m.DisplayName), int4ToNullable(m.ContextWindow),
		m.IsActive,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ModelRepo.Create: %w", err)
	}
	return domain.ID(id), nil
}

// GetActiveProviderForModel é o lookup do HOT PATH do proxy.
//
// Dado o nome do model solicitado pelo cliente (ex.: "deepseek-chat") e a org,
// retorna o PRIMEIRO provider ativo que oferece este model. Não há garantia de
// determinismo entre múltiplos providers com mesmo model — em F3 isto é OK
// (first-key-wins); em F5, o routing engine pode iterar providers para
// failover cross-provider (roadmap).
//
// Retorna (nil, ErrNotFound) se nenhum provider ativo oferece este model.
// O caller (KeyResolver) converte isto em domain.ErrModelNotFound.
func (r *ModelRepo) GetActiveProviderForModel(ctx context.Context, orgID domain.OrganizationID, modelName string) (*domain.Provider, error) {
	const q = `
		SELECT p.id, p.organization_id, p.name, p.slug, p.base_url, p.api_spec,
		       p.default_user_agent_override, p.is_active, p.created_at, p.updated_at
		FROM models m
		JOIN providers p ON p.id = m.provider_id
		WHERE m.organization_id = $1
		  AND m.name = $2
		  AND m.is_active = true
		  AND p.is_active = true
		ORDER BY p.id
		LIMIT 1`
	row := r.pool.QueryRow(ctx, q, int64(orgID), modelName)
	p, err := scanProvider(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("ModelRepo.GetActiveProviderForModel: %w", err)
	}
	return p, nil
}

// int4ToNullable converte *int para pgtype.Int4 (Valid=false se nil).
// Helper local (similar aos helpers.go, mas para Int4 aqui não exposto).
// Clamp em 2.1B previne overflow silencioso (gosec G115).
func int4ToNullable(i *int) pgtype.Int4 {
	if i == nil {
		return pgtype.Int4{Valid: false}
	}
	v := *i
	if v > 1<<31-1 {
		v = 1<<31 - 1
	}
	if v < -1<<31 {
		v = -1 << 31
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}
}

// ListAll retorna todos os modelos da organização (ativos e inativos).
func (r *ModelRepo) ListAll(ctx context.Context, orgID domain.OrganizationID) ([]domain.Model, error) {
	const q = `
		SELECT id, provider_id, organization_id, name, display_name, context_window,
		       model_aliases, model_pricing, modalities, is_active, created_at, updated_at
		FROM models
		WHERE organization_id = $1
		ORDER BY name`
	rows, err := r.pool.Query(ctx, q, int64(orgID))
	if err != nil {
		return nil, fmt.Errorf("ModelRepo.ListAll: %w", err)
	}
	defer rows.Close()

	var out []domain.Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, fmt.Errorf("ModelRepo.ListAll scan: %w", err)
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// GetByID carrega um modelo pela PK.
func (r *ModelRepo) GetByID(ctx context.Context, orgID domain.OrganizationID, id domain.ID) (*domain.Model, error) {
	const q = `
		SELECT id, provider_id, organization_id, name, display_name, context_window,
		       model_aliases, model_pricing, modalities, is_active, created_at, updated_at
		FROM models
		WHERE organization_id = $1 AND id = $2`
	row := r.pool.QueryRow(ctx, q, int64(orgID), int64(id))
	m, err := scanModel(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("ModelRepo.GetByID: %w", err)
	}
	return m, nil
}

// Update altera nome, display_name, context_window e is_active de um modelo.
// provider_id não é alterado (se necessário, recriar o modelo).
func (r *ModelRepo) Update(ctx context.Context, orgID domain.OrganizationID, m *domain.Model) error {
	if m == nil {
		return fmt.Errorf("ModelRepo.Update: nil model")
	}
	const q = `
		UPDATE models
		SET name = $3, display_name = $4, context_window = $5, is_active = $6, updated_at = now()
		WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q,
		int64(orgID), int64(m.ID), m.Name,
		ptrToText(m.DisplayName), int4ToNullable(m.ContextWindow), m.IsActive,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("ModelRepo.Update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetActive ativa ou inativa um modelo.
func (r *ModelRepo) SetActive(ctx context.Context, orgID domain.OrganizationID, id domain.ID, active bool) error {
	const q = `
		UPDATE models
		SET is_active = $3, updated_at = now()
		WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id), active)
	if err != nil {
		return fmt.Errorf("ModelRepo.SetActive: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete remove um modelo fisicamente.
func (r *ModelRepo) Delete(ctx context.Context, orgID domain.OrganizationID, id domain.ID) error {
	const q = `DELETE FROM models WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id))
	if err != nil {
		return fmt.Errorf("ModelRepo.Delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// scanModel mapeia coluna→domain.Model. Campos JSONB são descartados (MVP
// não os expõe na UI admin).
func scanModel(row pgx.Row) (*domain.Model, error) {
	var (
		m           domain.Model
		providerID  int64
		orgID       int64
		displayName pgtype.Text
		ctxWindow   pgtype.Int4
		_aliases    []byte
		_pricing    []byte
		_modalities []byte
	)
	err := row.Scan(
		&m.ID, &providerID, &orgID, &m.Name, &displayName, &ctxWindow,
		&_aliases, &_pricing, &_modalities, &m.IsActive, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	m.ProviderID = domain.ID(providerID)
	m.OrgID = domain.OrganizationID(orgID)
	m.DisplayName = textToPtr(displayName)
	if ctxWindow.Valid {
		cw := int(ctxWindow.Int32)
		m.ContextWindow = &cw
	}
	return &m, nil
}
