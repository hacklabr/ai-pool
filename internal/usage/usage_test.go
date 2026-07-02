package usage

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/jackc/pgx/v5/pgtype"
)

// =============================================================================
// Mocks para testes do collector e rollup job.
// =============================================================================

// mockUsageRepo captura InsertBatch/InsertDeadLetter para asserção.
type mockUsageRepo struct {
	mu         sync.Mutex
	batches    [][]postgres.UsageLogRecord
	dlCount    int
	dlPayloads [][]byte
	insertErr  error // se != nil, InsertBatch retorna erro
}

func (m *mockUsageRepo) InsertBatch(_ context.Context, records []postgres.UsageLogRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.insertErr != nil {
		return m.insertErr
	}
	cp := make([]postgres.UsageLogRecord, len(records))
	copy(cp, records)
	m.batches = append(m.batches, cp)
	return nil
}

func (m *mockUsageRepo) InsertDeadLetter(_ context.Context, payload any, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dlCount++
	// Captura o payload (pode ser []byte do json.Marshal).
	if b, ok := payload.([]byte); ok {
		m.dlPayloads = append(m.dlPayloads, b)
	} else if b, err := json.Marshal(payload); err == nil {
		m.dlPayloads = append(m.dlPayloads, b)
	}
	return nil
}

func (m *mockUsageRepo) batchCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.batches)
}

func (m *mockUsageRepo) totalRecords() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, b := range m.batches {
		n += len(b)
	}
	return n
}

// =============================================================================
// Collector — testes de batch por tamanho e por tempo.
// =============================================================================

func TestCollector_RecordNonBlocking(t *testing.T) {
	repo := &mockUsageRepo{}
	cfg := CollectorConfig{BufferSize: 5, BatchSize: 100, FlushInterval: 10 * time.Second}
	c := NewCollector(cfg, repo)

	// Sem Start, o canal aceita eventos mas nada é processado.
	for i := 0; i < 5; i++ {
		if ok := c.Record(domain.UsageEvent{UserID: domain.ID(i)}); !ok {
			t.Errorf("Record[%d] should succeed (canal não cheio)", i)
		}
	}
	// 6o evento — canal cheio (cap=5), deve ser descartado.
	if ok := c.Record(domain.UsageEvent{}); ok {
		t.Error("Record should fail (canal cheio)")
	}
	if c.DroppedCount() != 1 {
		t.Errorf("dropped = %d, want 1", c.DroppedCount())
	}
	if c.EnqueuedCount() != 5 {
		t.Errorf("enqueued = %d, want 5", c.EnqueuedCount())
	}
}

func TestCollector_BatchBySize(t *testing.T) {
	repo := &mockUsageRepo{}
	cfg := CollectorConfig{BufferSize: 100, BatchSize: 3, FlushInterval: 10 * time.Second}
	c := NewCollector(cfg, repo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	// Envia 6 eventos → deve gerar 2 batches (3+3).
	for i := 0; i < 6; i++ {
		c.Record(domain.UsageEvent{UserID: domain.ID(i)})
	}

	// Aguarda flush (select loop deve processar quando batch enche).
	time.Sleep(200 * time.Millisecond)

	if got := repo.batchCount(); got != 2 {
		t.Errorf("batchCount = %d, want 2", got)
	}
	if got := repo.totalRecords(); got != 6 {
		t.Errorf("totalRecords = %d, want 6", got)
	}

	cancel()
	c.Close()
}

func TestCollector_BatchByTime(t *testing.T) {
	repo := &mockUsageRepo{}
	cfg := CollectorConfig{BufferSize: 100, BatchSize: 100, FlushInterval: 50 * time.Millisecond}
	c := NewCollector(cfg, repo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	// Envia 2 eventos (abaixo do BatchSize=100). Deve ser flushed pelo timer.
	c.Record(domain.UsageEvent{UserID: 1})
	c.Record(domain.UsageEvent{UserID: 2})

	// Aguarda > FlushInterval.
	time.Sleep(200 * time.Millisecond)

	if got := repo.batchCount(); got < 1 {
		t.Errorf("batchCount = %d, want >= 1 (flushed by timer)", got)
	}
	if got := repo.totalRecords(); got != 2 {
		t.Errorf("totalRecords = %d, want 2", got)
	}

	cancel()
	c.Close()
}

func TestCollector_DeadLetterOnFailure(t *testing.T) {
	repo := &mockUsageRepo{insertErr: context.DeadlineExceeded}
	cfg := CollectorConfig{BufferSize: 100, BatchSize: 2, FlushInterval: 10 * time.Second}
	c := NewCollector(cfg, repo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	// Envia 2 eventos → batch enche → InsertBatch falha → dead-letter cada um.
	c.Record(domain.UsageEvent{UserID: 1})
	c.Record(domain.UsageEvent{UserID: 2})

	time.Sleep(200 * time.Millisecond)

	if repo.batchCount() != 0 {
		t.Errorf("batchCount = %d, want 0 (insert failed)", repo.batchCount())
	}
	if repo.dlCount != 2 {
		t.Errorf("dlCount = %d, want 2", repo.dlCount)
	}
	if len(repo.dlPayloads) != 2 {
		t.Errorf("dlPayloads len = %d, want 2", len(repo.dlPayloads))
	}

	cancel()
	c.Close()
}

func TestCollector_GracefulShutdownDrainsRemaining(t *testing.T) {
	repo := &mockUsageRepo{}
	cfg := CollectorConfig{BufferSize: 100, BatchSize: 100, FlushInterval: 10 * time.Second}
	c := NewCollector(cfg, repo)

	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)

	// Envia eventos que NÃO serão flushed por tamanho (batch=100) nem tempo.
	for i := 0; i < 5; i++ {
		c.Record(domain.UsageEvent{UserID: domain.ID(i)})
	}

	// Cancela contexto E fecha collector — deve drenar o pendente.
	cancel()
	c.Close()

	// O Close() deve ter flusheado os 5 eventos pendentes.
	if got := repo.totalRecords(); got != 5 {
		t.Errorf("totalRecords after Close = %d, want 5 (drained)", got)
	}
}

func TestCollector_ConcurrentRecord(t *testing.T) {
	// Garante thread-safety sob concorrência (-race).
	repo := &mockUsageRepo{}
	cfg := CollectorConfig{BufferSize: 1000, BatchSize: 100, FlushInterval: 50 * time.Millisecond}
	c := NewCollector(cfg, repo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	var wg sync.WaitGroup
	const N = 500
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			c.Record(domain.UsageEvent{UserID: 1})
		}()
	}
	wg.Wait()

	time.Sleep(200 * time.Millisecond)
	cancel()
	c.Close()

	// Nenhum panic; total processado + dropped == N.
	total := repo.totalRecords() + int(c.DroppedCount())
	if total != N {
		t.Errorf("total (processed+dropped) = %d, want %d", total, N)
	}
}

// =============================================================================
// Usage parser — testes de parse non-stream e stream.
// =============================================================================

func TestParseUsageFromBody_NonStream(t *testing.T) {
	body := []byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
	stats, ok := ParseUsageFromBody(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if stats.PromptTokens == nil || *stats.PromptTokens != 10 {
		t.Errorf("prompt = %v, want 10", stats.PromptTokens)
	}
	if stats.CompletionTokens == nil || *stats.CompletionTokens != 20 {
		t.Errorf("completion = %v, want 20", stats.CompletionTokens)
	}
	if stats.TotalTokens == nil || *stats.TotalTokens != 30 {
		t.Errorf("total = %v, want 30", stats.TotalTokens)
	}
}

func TestParseUsageFromBody_NoUsage(t *testing.T) {
	body := []byte(`{"id":"x","choices":[]}`)
	_, ok := ParseUsageFromBody(body)
	if ok {
		t.Error("expected ok=false when no usage field")
	}
}

func TestParseUsageFromBody_CachedTokensRoot(t *testing.T) {
	// DeepSeek formato: cached_tokens no nível raiz do usage.
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"cached_tokens":60}}`)
	stats, ok := ParseUsageFromBody(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if stats.CachedTokens == nil || *stats.CachedTokens != 60 {
		t.Errorf("cached = %v, want 60", stats.CachedTokens)
	}
}

func TestParseUsageFromBody_CachedTokensNested(t *testing.T) {
	// OpenAI novo formato: prompt_tokens_details.cached_tokens.
	body := []byte(`{"usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":70}}}`)
	stats, ok := ParseUsageFromBody(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if stats.CachedTokens == nil || *stats.CachedTokens != 70 {
		t.Errorf("cached = %v, want 70", stats.CachedTokens)
	}
}

func TestParseUsageFromStreamBytes_FinalChunkHasUsage(t *testing.T) {
	stream := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
			"data: [DONE]\n\n",
	)
	stats, ok := ParseUsageFromStreamBody(stream)
	if !ok {
		t.Fatal("expected ok (usage in last chunk)")
	}
	if stats.PromptTokens == nil || *stats.PromptTokens != 5 {
		t.Errorf("prompt = %v, want 5", stats.PromptTokens)
	}
	if stats.TotalTokens == nil || *stats.TotalTokens != 7 {
		t.Errorf("total = %v, want 7", stats.TotalTokens)
	}
}

func TestParseUsageFromStreamBytes_NoUsage(t *testing.T) {
	stream := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	_, ok := ParseUsageFromStreamBody(stream)
	if ok {
		t.Error("expected ok=false (no usage in stream)")
	}
}

func TestParseUsageFromStreamBytes_EmptyBody(t *testing.T) {
	_, ok := ParseUsageFromStreamBody(strings.NewReader(""))
	if ok {
		t.Error("expected ok=false for empty body")
	}
}

// =============================================================================
// EventToRecord — conversão UsageEvent → UsageLogRecord.
// =============================================================================

func TestEventToRecord_MapsFields(t *testing.T) {
	pt, ct, ch, tt := 10, 20, 5, 30
	sc, lat := 200, int(150)
	ev := domain.UsageEvent{
		OrganizationID:   1,
		UserID:           2,
		TokenID:          3,
		ProviderID:       4,
		KeyID:            5,
		Model:            "test-model",
		Stream:           true,
		PromptTokens:     &pt,
		CompletionTokens: &ct,
		CachedTokens:     &ch,
		TotalTokens:      &tt,
		LatencyMS:        &lat,
		StatusCode:       &sc,
	}
	rec := EventToRecord(ev)
	if rec.OrganizationID != 1 || rec.UserID != 2 || rec.TokenID != 3 {
		t.Errorf("IDs wrong: %+v", rec)
	}
	if rec.Model != "test-model" {
		t.Errorf("model = %q", rec.Model)
	}
	if !rec.Stream {
		t.Error("stream should be true")
	}
	if *rec.PromptTokens != 10 || *rec.CompletionTokens != 20 {
		t.Errorf("tokens wrong")
	}
	if rec.CachedTokens != 5 {
		t.Errorf("cached_tokens = %d, want 5", rec.CachedTokens)
	}
	if *rec.TotalTokens != 30 {
		t.Errorf("total = %v, want 30", rec.TotalTokens)
	}
}

func TestEventToRecord_NilCachedTokensDefaultsZero(t *testing.T) {
	ev := domain.UsageEvent{CachedTokens: nil}
	rec := EventToRecord(ev)
	if rec.CachedTokens != 0 {
		t.Errorf("cached_tokens = %d, want 0 (nil→0)", rec.CachedTokens)
	}
}

// =============================================================================
// RollupJob — teste do interval e formato de janela.
// =============================================================================

// mockRollupRepo captura AggregateHourFromLogs/UpsertRollups.
type mockRollupRepo struct {
	aggregateCalls atomic.Int64
	upsertRows     []postgres.RollupRow
	upsertCalls    atomic.Int64
	returnRows     []postgres.RollupRow
	errOnAggregate error
}

func (m *mockRollupRepo) AggregateHourFromLogs(_ context.Context, _ int64, _, _ pgtype.Timestamptz) ([]postgres.RollupRow, error) {
	m.aggregateCalls.Add(1)
	if m.errOnAggregate != nil {
		return nil, m.errOnAggregate
	}
	return m.returnRows, nil
}

func (m *mockRollupRepo) UpsertRollups(_ context.Context, rows []postgres.RollupRow) error {
	m.upsertCalls.Add(1)
	m.upsertRows = rows
	return nil
}

func TestRollupJob_RunOnce_NoData(t *testing.T) {
	repo := &mockRollupRepo{returnRows: nil}
	logger := slogDiscard()
	j := NewRollupJob(repo, 1, time.Minute, logger)

	err := j.RunOnce(context.Background())
	if err != nil {
		t.Errorf("err = %v, want nil (no data)", err)
	}
	if repo.upsertCalls.Load() != 0 {
		t.Error("UpsertRollups should NOT be called when no data")
	}
}

func TestRollupJob_RunOnce_WithData(t *testing.T) {
	repo := &mockRollupRepo{returnRows: []postgres.RollupRow{
		{OrganizationID: 1, UserID: 1, ProviderID: 1, Model: "test",
			RequestCount: 10, PromptTokens: 100, CompletionTokens: 50,
			CachedTokens: 30, TotalTokens: 150, ErrorCount: 1, TotalLatencyMS: 5000},
	}}
	logger := slogDiscard()
	j := NewRollupJob(repo, 1, time.Minute, logger)

	err := j.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if repo.aggregateCalls.Load() != 1 {
		t.Errorf("aggregate calls = %d, want 1", repo.aggregateCalls.Load())
	}
	if repo.upsertCalls.Load() != 1 {
		t.Errorf("upsert calls = %d, want 1", repo.upsertCalls.Load())
	}
	if len(repo.upsertRows) != 1 {
		t.Errorf("upsert rows = %d, want 1", len(repo.upsertRows))
	}
	if repo.upsertRows[0].CachedTokens != 30 {
		t.Errorf("cached = %d, want 30", repo.upsertRows[0].CachedTokens)
	}
}

// === helpers ===

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
