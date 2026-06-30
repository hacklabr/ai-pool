package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/hacklabr/ai-pool/internal/domain"
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
