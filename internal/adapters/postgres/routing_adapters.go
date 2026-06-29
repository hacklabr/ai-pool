package postgres

import (
	"context"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/routing"
)

// =============================================================================
// Adapters: repositórios PostgreSQL → portas do routing engine.
//
// O routing engine depende de abstrações (EngineDeps, SchedulerDeps,
// KeyRepoPort) para permitir testes com mocks. Em produção, estas portas são
// implementadas por composição dos repositórios postgres.
// =============================================================================

// Compile-time assertions: garantem que os structs satisfazem as interfaces.
var (
	_ routing.EngineDeps    = (*EngineDepsAdapter)(nil)
	_ routing.SchedulerDeps = (*SchedulerDepsAdapter)(nil)
	_ routing.KeyRepoPort   = (*KeyRepo)(nil)
)

// EngineDepsAdapter adapta ModelRepo + KeyRepo para routing.EngineDeps.
type EngineDepsAdapter struct {
	ModelRepo *ModelRepo
	KeyRepo   *KeyRepo
}

// NewEngineDepsAdapter monta o adapter.
func NewEngineDepsAdapter(modelRepo *ModelRepo, keyRepo *KeyRepo) *EngineDepsAdapter {
	return &EngineDepsAdapter{ModelRepo: modelRepo, KeyRepo: keyRepo}
}

// GetActiveProviderForModel delega para ModelRepo.
func (a *EngineDepsAdapter) GetActiveProviderForModel(ctx context.Context, orgID domain.OrganizationID, model string) (*domain.Provider, error) {
	p, err := a.ModelRepo.GetActiveProviderForModel(ctx, orgID, model)
	if err != nil {
		if isNoRows(err) {
			return nil, domain.ErrModelNotFound
		}
		return nil, err
	}
	return p, nil
}

// ListUsableByProvider delega para KeyRepo.
func (a *EngineDepsAdapter) ListUsableByProvider(ctx context.Context, orgID domain.OrganizationID, providerID domain.ID) ([]domain.ProviderKey, error) {
	return a.KeyRepo.ListUsableByProvider(ctx, orgID, providerID)
}

// ReactivateExpiredKeys delega para KeyRepo.ReactivateExpiredKeys.
func (a *EngineDepsAdapter) ReactivateExpiredKeys(ctx context.Context, orgID domain.OrganizationID, now time.Time) ([]domain.ID, error) {
	return a.KeyRepo.ReactivateExpiredKeys(ctx, orgID, now)
}

// SchedulerDepsAdapter adapta KeyRepo para routing.SchedulerDeps.
type SchedulerDepsAdapter struct {
	KeyRepo *KeyRepo
}

// NewSchedulerDepsAdapter monta o adapter.
func NewSchedulerDepsAdapter(keyRepo *KeyRepo) *SchedulerDepsAdapter {
	return &SchedulerDepsAdapter{KeyRepo: keyRepo}
}

// ListToReactivate delega para KeyRepo e converte o tipo retornado.
func (a *SchedulerDepsAdapter) ListToReactivate(ctx context.Context, orgID domain.OrganizationID, limit int) ([]routing.ReactivationCandidate, error) {
	cands, err := a.KeyRepo.ListToReactivate(ctx, orgID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]routing.ReactivationCandidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, routing.ReactivationCandidate{
			KeyID:      c.KeyID,
			ProviderID: c.ProviderID,
		})
	}
	return out, nil
}

// Reactivate delega para KeyRepo.
func (a *SchedulerDepsAdapter) Reactivate(ctx context.Context, orgID domain.OrganizationID, keyID domain.ID) error {
	return a.KeyRepo.Reactivate(ctx, orgID, keyID)
}
