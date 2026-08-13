package modelsync

import (
	"context"
	"log/slog"
	"time"
)

// =============================================================================
// Job — goroutine periódica que roda SyncAll (padrão usage.RollupJob).
//
// Lifecycle: inicia no main (`go job.Start(ctx)`), para no shutdown (cancel
// ctx). O sync inicial roda IMEDIATAMENTE dentro de Start — como o caller
// sempre invoca Start em goroutine, o boot/readiness nunca é bloqueado
// (fail-open). Falhas de provedores individuais não afetam o job (fail-safe,
// ADR-0015); falha estrutural (DB) é logada e o próximo tick tenta de novo.
//
// Intervalo: configurável via AIPP_MODEL_SYNC_INTERVAL (default 6h); o valor
// 0 é tratado no wiring do main (job nem é criado/iniciado).
// =============================================================================

// Job roda a sincronização do catálogo periodicamente.
type Job struct {
	syncer   *Syncer
	interval time.Duration
	logger   *slog.Logger
}

// NewJob cria o job periódico. interval <= 0 recebe o default de 6h
// (defensivo — o wiring normal nunca cria o job quando o intervalo é 0).
func NewJob(syncer *Syncer, interval time.Duration, logger *slog.Logger) *Job {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	return &Job{syncer: syncer, interval: interval, logger: logger}
}

// Start inicia o loop. Retorna apenas quando ctx é cancelado; o caller deve
// invocar em goroutine (`go job.Start(ctx)`).
func (j *Job) Start(ctx context.Context) {
	j.logger.Info("model sync job started", "interval", j.interval)
	// Sync inicial imediato: instalação/restart já nasce com o catálogo
	// populado, sem esperar o primeiro tick.
	j.runOnce(ctx)
	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			j.logger.Info("model sync job stopped")
			return
		case <-ticker.C:
			j.runOnce(ctx)
		}
	}
}

// RunOnce executa uma rodada de sincronização (exposto para testes e para o
// CLI --sync-models via Syncer.SyncAll).
func (j *Job) RunOnce(ctx context.Context) (*Report, error) {
	return j.syncer.SyncAll(ctx)
}

func (j *Job) runOnce(ctx context.Context) {
	if _, err := j.syncer.SyncAll(ctx); err != nil {
		// Falha estrutural (ex.: DB indisponível) — loga e aguarda o próximo
		// tick; o job nunca morre por causa de uma rodada ruim.
		j.logger.Warn("model sync round failed", "err", err)
	}
}
