// Package usage implementa o registro de uso assíncrono (F6).
//
// Componentes:
//   - Collector: canal bufferizado + goroutine worker que faz batch insert.
//     Record() é NÃO-BLOQUEANTE (descarta se canal cheio).
//   - ParseUsage: extrai tokens do response do provedor (non-stream JSON +
//     stream SSE final chunk).
//   - RollupJob: goroutine periódica que agrega usage_logs em usage_rollups
//     (KPI de cached_tokens para dashboard).
package usage

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Collector — coleta eventos via canal bufferizado e persiste em batches.
// =============================================================================

// CollectorConfig parametriza o coletor.
type CollectorConfig struct {
	BufferSize    int           // capacidade do canal (default 10000)
	BatchSize     int           // flush a cada N eventos (default 100)
	FlushInterval time.Duration // flush a cada D (default 1s)
}

// DefaultCollectorConfig retorna defaults da spec.
func DefaultCollectorConfig() CollectorConfig {
	return CollectorConfig{
		BufferSize:    10000,
		BatchSize:     100,
		FlushInterval: 1 * time.Second,
	}
}

// UsageRepoPort é a porta mínima do repositório (para mock).
type UsageRepoPort interface {
	InsertBatch(ctx context.Context, records []postgres.UsageLogRecord) error
	InsertDeadLetter(ctx context.Context, payload any, errMsg string) error
}

// Collector implementa domain.UsageCollector.
type Collector struct {
	cfg    CollectorConfig
	repo   UsageRepoPort
	events chan domain.UsageEvent

	// Métricas (atomic — thread-safe sem mutex).
	droppedCount  atomic.Int64
	enqueuedCount atomic.Int64

	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// NewCollector cria o coletor.
func NewCollector(cfg CollectorConfig, repo UsageRepoPort) *Collector {
	return &Collector{
		cfg:    cfg,
		repo:   repo,
		events: make(chan domain.UsageEvent, cfg.BufferSize),
	}
}

// Record enfileira o evento para persistência. NÃO-BLOQUEANTE.
//
// Se o canal está cheio, o evento é DESCARTADO e droppedCount é incrementado.
// Isto garante que o path crítico do proxy NUNCA é bloqueado por persistência
// lenta. A métrica de drop permite detectar underprovisioning.
func (c *Collector) Record(event domain.UsageEvent) bool {
	select {
	case c.events <- event:
		c.enqueuedCount.Add(1)
		return true
	default:
		c.droppedCount.Add(1)
		return false
	}
}

// Start inicia a goroutine consumidora. Idempotente (safe chamar múltiplas vezes).
func (c *Collector) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.wg.Add(1)
		go c.run(ctx)
	})
}

// Close fecha o canal, drena o pendente e espera o worker terminar.
// Deve ser chamado APÓS o contexto de Start ser cancelado.
func (c *Collector) Close() error {
	c.stopOnce.Do(func() {
		close(c.events)
	})
	c.wg.Wait()
	return nil
}

// DroppedCount retorna quantos eventos foram descartados (canal cheio).
func (c *Collector) DroppedCount() int64 { return c.droppedCount.Load() }

// EnqueuedCount retorna quantos eventos foram enfileirados com sucesso.
func (c *Collector) EnqueuedCount() int64 { return c.enqueuedCount.Load() }

// run é a goroutine principal do worker.
//
// Loop: a cada BatchSize eventos OU FlushInterval (o que vier primeiro),
// faz InsertBatch. Em caso de erro, InsertDeadLetter para CADA evento do
// batch (preserva o payload para reprocessamento).
func (c *Collector) run(ctx context.Context) {
	defer c.wg.Done()

	batch := make([]domain.UsageEvent, 0, c.cfg.BatchSize)
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.flushBatch(ctx, batch)
		batch = batch[:0] // reusa capacidade
	}

	for {
		select {
		case <-ctx.Done():
			// Drena o restante do canal antes de sair (graceful).
			// Como Close() fecha o channel, o range abaixo será usado.
			flush()
			// Drena eventos que entraram entre o cancelamento e o close.
			for ev := range c.events {
				batch = append(batch, ev)
				if len(batch) >= c.cfg.BatchSize {
					flush()
				}
			}
			flush()
			return

		case ev, ok := <-c.events:
			if !ok {
				// Canal fechado (Close chamado). Drena e sai.
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= c.cfg.BatchSize {
				flush()
			}

		case <-ticker.C:
			flush()
		}
	}
}

// flushBatch converte eventos → UsageLogRecord e tenta InsertBatch.
// Em caso de falha, envia CADA evento para dead-letter individualmente
// (preserva payload; permite reprocessamento seletivo).
func (c *Collector) flushBatch(ctx context.Context, events []domain.UsageEvent) {
	records := make([]postgres.UsageLogRecord, 0, len(events))
	for _, ev := range events {
		records = append(records, EventToRecord(ev))
	}
	if err := c.repo.InsertBatch(ctx, records); err != nil {
		// Dead-letter cada evento individualmente com o payload original.
		// O payload é o UsageEvent serializado como JSON (para reprocessamento).
		for _, ev := range events {
			payload, _ := json.Marshal(ev)
			if dlErr := c.repo.InsertDeadLetter(ctx, payload, err.Error()); dlErr != nil {
				// Dead-letter também falhou — situação extrema (DB indisponível).
				// Logamos e aceitamos a perda (WAL protege o que já está no DB).
				_ = dlErr
			}
		}
	}
}

// EventToRecord converte domain.UsageEvent → postgres.UsageLogRecord.
// Centraliza o mapeamento para evitar duplicação entre collector e testes.
func EventToRecord(ev domain.UsageEvent) postgres.UsageLogRecord {
	return postgres.UsageLogRecord{
		OrganizationID:   int64(ev.OrganizationID),
		UserID:           int64(ev.UserID),
		TokenID:          int64(ev.TokenID),
		ProviderID:       int64(ev.ProviderID),
		KeyID:            int64(ev.KeyID),
		Model:            ev.Model,
		Stream:           ev.Stream,
		PromptTokens:     ev.PromptTokens,
		CompletionTokens: ev.CompletionTokens,
		CachedTokens:     intFromPtr(ev.CachedTokens),
		TotalTokens:      ev.TotalTokens,
		LatencyMS:        ev.LatencyMS,
		StatusCode:       ev.StatusCode,
		ErrorClass:       ev.ErrorClass,
	}
}

// intFromPtr retorna 0 se nil (para cached_tokens NOT NULL DEFAULT 0 no schema).
func intFromPtr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
