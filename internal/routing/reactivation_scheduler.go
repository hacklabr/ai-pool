package routing

import (
	"context"
	"log/slog"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/observability"
)

// =============================================================================
// ReactivationScheduler — goroutine periódica que reativa chaves cujo
// disabled_until já passou.
//
// Spec §3.2.4:
//   - Tick periódico (default 30s).
//   - KeyRepo.ListToReactivate usa SELECT ... FOR UPDATE SKIP LOCKED (multi-
//     worker safe).
//   - KeyRepo.Reactivate marca status=enabled, zera error_count.
//   - LRUStore.OnKeyReenabled reinsere a chave no pool de candidatas.
//
// Lifecycle:
//   - Start(ctx): inicia goroutine que roda até ctx cancelado (shutdown).
//   - Stop(): cancelável via contexto passado em Start.
// =============================================================================

// SchedulerDeps porta mínima dos repositórios necessários ao scheduler.
type SchedulerDeps interface {
	ListToReactivate(ctx context.Context, orgID domain.OrganizationID, limit int) ([]ReactivationCandidate, error)
	Reactivate(ctx context.Context, orgID domain.OrganizationID, keyID domain.ID) error
}

// ReactivationCandidate é o resultado de ListToReactivate.
type ReactivationCandidate struct {
	KeyID      domain.ID
	ProviderID domain.ID
}

// ReactivationScheduler reativa chaves cujo disabled_until passou.
type ReactivationScheduler struct {
	deps     SchedulerDeps
	lru      domain.LRUStore
	orgID    domain.OrganizationID
	interval time.Duration
	limit    int
	logger   *slog.Logger
	// auditor registra reativações no audit_log (issue #19, RF-03). nil-safe.
	auditor KeyTransitionAuditor
}

// NewReactivationScheduler cria o scheduler.
func NewReactivationScheduler(
	deps SchedulerDeps,
	lru domain.LRUStore,
	orgID domain.OrganizationID,
	interval time.Duration,
	logger *slog.Logger,
) *ReactivationScheduler {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &ReactivationScheduler{
		deps:     deps,
		lru:      lru,
		orgID:    orgID,
		interval: interval,
		limit:    50,
		logger:   logger,
	}
}

// SetTransitionAuditor injeta o auditor de transições (issue #19). Opcional
// (nil-safe): sem auditor, as reativações não geram eventos de audit_log.
func (s *ReactivationScheduler) SetTransitionAuditor(a KeyTransitionAuditor) {
	s.auditor = a
}

// Start inicia a goroutine de reativação. Retorna imediatamente; roda até
// ctx ser cancelado (em shutdown).
//
// Uso típico:
//
//	schedCtx, cancel := context.WithCancel(bgCtx)
//	go scheduler.Start(schedCtx)
//	// ... em shutdown:
//	cancel()
func (s *ReactivationScheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.logger.Info("reactivation scheduler started",
		"interval", s.interval, "limit", s.limit)
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("reactivation scheduler stopped")
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

// RunOnce executa uma rodada de reativação (exposta para testes).
func (s *ReactivationScheduler) RunOnce(ctx context.Context) (int, error) {
	return s.runOnce(ctx)
}

func (s *ReactivationScheduler) runOnce(ctx context.Context) (int, error) {
	cands, err := s.deps.ListToReactivate(ctx, s.orgID, s.limit)
	if err != nil {
		s.logger.Warn("reactivation: ListToReactivate failed", "err", err)
		return 0, err
	}
	if len(cands) == 0 {
		return 0, nil
	}
	reactivated := 0
	for _, c := range cands {
		if err := s.deps.Reactivate(ctx, s.orgID, c.KeyID); err != nil {
			s.logger.Warn("reactivation: Reactivate failed",
				"key_id", c.KeyID, "err", err)
			continue
		}
		// Notifica LRUStore que a chave voltou ao pool.
		if s.lru != nil {
			_ = s.lru.OnKeyReenabled(ctx, c.KeyID, c.ProviderID)
		}
		// Auditoria da reativação (issue #19, RF-03). Best-effort.
		if s.auditor != nil {
			if err := s.auditor.AppendKeyTransition(ctx, s.orgID, c.KeyID, AuditActionReactivate,
				map[string]any{"source": "scheduler"}); err != nil {
				s.logger.Warn("reactivation: audit append failed", "key_id", c.KeyID, "err", err)
			}
		}
		// Métrica Prometheus (F11): uma reativação = um inc.
		observability.IncKeyReactivation()
		reactivated++
	}
	if reactivated > 0 {
		s.logger.Info("reactivation: keys reactivated", "count", reactivated)
	}
	return reactivated, nil
}
