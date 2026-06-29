package usage

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
)

// =============================================================================
// RollupJob — goroutine periódica que agrega usage_logs em usage_rollups.
//
// Spec §3.4.2:
//   - Agrega a última hora FECHADA (não a hora corrente, que ainda está
//     recebendo inserts).
//   - UpsertRollups é idempotente (ON CONFLICT DO UPDATE) — rodar de novo
//     para a mesma hora apenas soma incrementalmente.
//   - Rollups alimentam o dashboard com KPI de cached_tokens.
//
// Frequência (spec): a cada hora para a hora fechada. Mas para reduzir
// latência do dashboard, fazemos tick a cada 5min agregando a última hora
// fechada. UpsertRollups é idempotente — agregações repetidas somam corretamente.
//
// Lifecycle: inicia no main (Start), para no shutdown (cancel ctx).
// =============================================================================

// RollupJobPort é a porta mínima do repositório (para mock).
type RollupJobPort interface {
	AggregateHourFromLogs(ctx context.Context, orgID int64, from, to pgtype.Timestamptz) ([]postgres.RollupRow, error)
	UpsertRollups(ctx context.Context, rows []postgres.RollupRow) error
}

// RollupJob roda agregação periódica.
type RollupJob struct {
	repo     RollupJobPort
	orgID    int64
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// NewRollupJob cria o job. interval default 1min se <= 0 (agrega a hora
// corrente a cada tick para o dashboard/Meu uso ver dados recentes).
func NewRollupJob(
	repo RollupJobPort,
	orgID int64,
	interval time.Duration,
	logger *slog.Logger,
) *RollupJob {
	if interval <= 0 {
		interval = time.Minute
	}
	return &RollupJob{
		repo:     repo,
		orgID:    orgID,
		interval: interval,
		logger:   logger,
		now:      time.Now,
	}
}

// Start inicia a goroutine. Retorna imediatamente; roda até ctx cancelado.
func (j *RollupJob) Start(ctx context.Context) {
	j.logger.Info("rollup job started", "interval", j.interval, "org_id", j.orgID)
	// Roda uma vez IMEDIATAMENTE para que dados recentes apareçam sem esperar o
	// primeiro tick (importante após deploy/restart e para feedback rápido).
	j.runOnce(ctx)
	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			j.logger.Info("rollup job stopped")
			return
		case <-ticker.C:
			j.runOnce(ctx)
		}
	}
}

// RunOnce executa uma rodada de agregação (exposta para testes).
// Agrega a última hora FECHADA (do início da hora anterior ao início da hora atual).
func (j *RollupJob) RunOnce(ctx context.Context) error {
	return j.runOnce(ctx)
}

func (j *RollupJob) runOnce(ctx context.Context) error {
	now := j.now().UTC()
	// Início da hora atual = floor(now, 1h).
	currentHourStart := now.Truncate(time.Hour)
	// Início da hora anterior = fechada (pronta para agregar).
	prevHourStart := currentHourStart.Add(-time.Hour)

	from := pgtype.Timestamptz{Time: prevHourStart, Valid: true}
	// Agrega até AGORA — inclui a hora CORRENTE (parcial). Como UpsertRollups é
	// idempotente (ON CONFLICT DO UPDATE), a hora corrente é atualizada a cada
	// tick conforme novos logs chegam, e o "Meu uso"/dashboard vê dados recentes
	// sem esperar a hora virar.
	to := pgtype.Timestamptz{Time: now, Valid: true}

	rows, err := j.repo.AggregateHourFromLogs(ctx, j.orgID, from, to)
	if err != nil {
		j.logger.Warn("rollup: AggregateHourFromLogs failed", "err", err, "from", prevHourStart, "to", now)
		return err
	}
	if len(rows) == 0 {
		// Sem dados na última hora — normal em baixo tráfego.
		return nil
	}
	if err := j.repo.UpsertRollups(ctx, rows); err != nil {
		j.logger.Warn("rollup: UpsertRollups failed", "err", err, "rows", len(rows))
		return err
	}
	// Loga apenas contagem agregada (não tokens individuais — evitar ruído).
	totalReq := int64(0)
	totalCached := int64(0)
	for _, r := range rows {
		totalReq += r.RequestCount
		totalCached += r.CachedTokens
	}
	j.logger.Info("rollup: aggregated", "rows", len(rows), "requests", totalReq, "cached_tokens", totalCached)
	return nil
}
