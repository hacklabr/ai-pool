package routing

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Mocks e fakes para testes do routing engine.
// =============================================================================

// fakeKeyRepoPort implementa routing.KeyRepoPort para testes.
type fakeKeyRepoPort struct {
	mu                sync.Mutex
	transitions       int // quantas vezes TransitionToAutoDisabled foi chamado
	lastReason        domain.KeyDisabledReason
	lastDisabledUntil time.Time
	lastErrorCount    int
	returnErr         error
}

func (f *fakeKeyRepoPort) TransitionToAutoDisabled(
	_ context.Context,
	_ domain.OrganizationID,
	_ domain.ID,
	reason domain.KeyDisabledReason,
	disabledUntil time.Time,
	errorCount int,
	_ int,
	_ *string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.returnErr != nil {
		return f.returnErr
	}
	f.transitions++
	f.lastReason = reason
	f.lastDisabledUntil = disabledUntil
	f.lastErrorCount = errorCount
	return nil
}

// fakeLRUStore captura chamadas OnKeyDisabled/OnKeyReenabled para asserção.
type fakeLRUStore struct {
	disabled  map[domain.ID]bool
	reenabled map[domain.ID]bool
	markUsed  map[domain.ID]int
}

func newFakeLRUStore() *fakeLRUStore {
	return &fakeLRUStore{
		disabled:  make(map[domain.ID]bool),
		reenabled: make(map[domain.ID]bool),
		markUsed:  make(map[domain.ID]int),
	}
}

func (f *fakeLRUStore) PickLeastUsed(_ context.Context, _ domain.ID, _ []domain.ID) (*domain.KeyCandidate, error) {
	return nil, domain.ErrNoKeysAvailable
}
func (f *fakeLRUStore) MarkUsed(_ context.Context, keyID domain.ID) error {
	f.markUsed[keyID]++
	return nil
}
func (f *fakeLRUStore) OnKeyDisabled(_ context.Context, keyID domain.ID) error {
	f.disabled[keyID] = true
	return nil
}
func (f *fakeLRUStore) OnKeyReenabled(_ context.Context, keyID domain.ID, _ domain.ID) error {
	f.reenabled[keyID] = true
	return nil
}

func (f *fakeLRUStore) EnsureCandidate(_ context.Context, _ domain.ID, _ domain.ID) error {
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeAuditor captura eventos de auditoria para asserção (issue #19).
type fakeAuditor struct {
	mu      sync.Mutex
	events  []string
	metas   []map[string]any
	failErr error
}

func (f *fakeAuditor) AppendKeyTransition(
	_ context.Context,
	_ domain.OrganizationID,
	_ domain.ID,
	action string,
	metadata any,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	f.events = append(f.events, action)
	if m, ok := metadata.(map[string]any); ok {
		f.metas = append(f.metas, m)
	}
	return nil
}

// =============================================================================
// AutoDisableManager
// =============================================================================

func newAutoDisableManagerForTest(t *testing.T, now time.Time) (*AutoDisableManager, *fakeKeyRepoPort, *fakeLRUStore) {
	t.Helper()
	fakeRepo := &fakeKeyRepoPort{}
	fakeLRU := newFakeLRUStore()
	parsers := NewParserRegistry()
	cfg := AutoDisableConfig{
		Threshold:    5,
		Window:       2 * time.Minute,
		BackoffSteps: []time.Duration{1 * time.Minute, 5 * time.Minute, 15 * time.Minute},
	}
	m := NewAutoDisableManager(cfg, parsers, fakeRepo, fakeLRU, discardLogger())
	m.now = func() time.Time { return now }
	return m, fakeRepo, fakeLRU
}

func TestAutoDisable_BelowThreshold_NoTransition(t *testing.T) {
	now := time.Now()
	m, fakeRepo, fakeLRU := newAutoDisableManagerForTest(t, now)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	// 4 erros — abaixo do threshold (5). NÃO transita.
	for i := 0; i < 4; i++ {
		if err := m.RecordError(ctx, org, key, 1, "deepseek", 429, []byte(`rate limited`)); err != nil {
			t.Fatalf("RecordError[%d]: %v", i, err)
		}
	}
	if fakeRepo.transitions != 0 {
		t.Errorf("transitions = %d, want 0 (below threshold)", fakeRepo.transitions)
	}
	if fakeLRU.disabled[key] {
		t.Error("LRU should NOT be notified (not disabled)")
	}
}

func TestAutoDisable_AtThreshold_TriggersTransition(t *testing.T) {
	now := time.Now()
	m, fakeRepo, fakeLRU := newAutoDisableManagerForTest(t, now)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	// 5 erros dentro da janela → transita.
	for i := 0; i < 5; i++ {
		_ = m.RecordError(ctx, org, key, 1, "deepseek", 429, []byte(`rate limited`))
	}
	if fakeRepo.transitions != 1 {
		t.Errorf("transitions = %d, want 1", fakeRepo.transitions)
	}
	if !fakeLRU.disabled[key] {
		t.Error("LRU should be notified of disable")
	}
}

func TestAutoDisable_ParsesDateFromErrorBody(t *testing.T) {
	now := time.Now()
	m, fakeRepo, _ := newAutoDisableManagerForTest(t, now)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	futureDate := now.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	body := []byte(`rate limit exceeded; retry after ` + futureDate)

	for i := 0; i < 5; i++ {
		_ = m.RecordError(ctx, org, key, 1, "deepseek", 429, body)
	}

	if fakeRepo.lastReason != domain.ReasonQuotaExceeded {
		t.Errorf("reason = %q, want quota_exceeded", fakeRepo.lastReason)
	}
	// disabledUntil deve casar com futureDate (com tolerância de 1s).
	if d := fakeRepo.lastDisabledUntil.Sub(now); d < 1*time.Hour || d > 3*time.Hour {
		t.Errorf("disabledUntil delta = %v, want ~2h", d)
	}
}

func TestAutoDisable_ParseFails_UsesBackoff(t *testing.T) {
	now := time.Now()
	m, fakeRepo, _ := newAutoDisableManagerForTest(t, now)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	// Body sem data reconhecível → backoff crescente.
	body := []byte(`something went wrong`)

	for i := 0; i < 5; i++ {
		_ = m.RecordError(ctx, org, key, 1, "deepseek", 429, body)
	}

	if fakeRepo.lastReason != domain.ReasonParseFailedRetry {
		t.Errorf("reason = %q, want parse_failed_retry", fakeRepo.lastReason)
	}
	// Primeiro disable → BackoffSteps[0] = 1min.
	if d := fakeRepo.lastDisabledUntil.Sub(now); d < 50*time.Second || d > 70*time.Second {
		t.Errorf("backoff delta = %v, want ~1min", d)
	}
}

func TestAutoDisable_BackoffGrowsAcrossCycles(t *testing.T) {
	now := time.Now()
	m, fakeRepo, _ := newAutoDisableManagerForTest(t, now)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	// Ciclo 1: 5 erros → disable com backoff[0]=1min.
	for i := 0; i < 5; i++ {
		_ = m.RecordError(ctx, org, key, 1, "deepseek", 429, []byte(`fail`))
	}
	if d := fakeRepo.lastDisabledUntil.Sub(now); d > 70*time.Second {
		t.Errorf("ciclo 1 delta = %v, want ~1min", d)
	}

	// Ciclo 2: mais 5 erros → disable com backoff[1]=5min.
	for i := 0; i < 5; i++ {
		_ = m.RecordError(ctx, org, key, 1, "deepseek", 429, []byte(`fail`))
	}
	if d := fakeRepo.lastDisabledUntil.Sub(now); d < 4*time.Minute || d > 6*time.Minute {
		t.Errorf("ciclo 2 delta = %v, want ~5min", d)
	}
}

func TestAutoDisable_ResetCounterOnSuccess(t *testing.T) {
	now := time.Now()
	m, fakeRepo, _ := newAutoDisableManagerForTest(t, now)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	// 4 erros (abaixo do threshold).
	for i := 0; i < 4; i++ {
		_ = m.RecordError(ctx, org, key, 1, "deepseek", 429, []byte(`fail`))
	}
	// Sucesso reseta contador.
	m.ResetCounter(org, key)
	// Mais 4 erros — total acumulado seria 8 mas reset zerou → não atinge 5.
	for i := 0; i < 4; i++ {
		_ = m.RecordError(ctx, org, key, 1, "deepseek", 429, []byte(`fail`))
	}
	if fakeRepo.transitions != 0 {
		t.Errorf("transitions = %d, want 0 (counter reset)", fakeRepo.transitions)
	}
}

func TestAutoDisable_WindowExpiryResets(t *testing.T) {
	// Testa que contagem zera após janela expirar.
	now := time.Now()
	m, fakeRepo, _ := newAutoDisableManagerForTest(t, now)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	// 4 erros.
	for i := 0; i < 4; i++ {
		_ = m.RecordError(ctx, org, key, 1, "deepseek", 429, []byte(`fail`))
	}

	// Avança o clock para depois da janela (3min > 2min).
	m.now = func() time.Time { return now.Add(3 * time.Minute) }

	// Mais 4 erros — total seria 8 mas janela resetou → 4. Não atinge 5.
	for i := 0; i < 4; i++ {
		_ = m.RecordError(ctx, org, key, 1, "deepseek", 429, []byte(`fail`))
	}
	if fakeRepo.transitions != 0 {
		t.Errorf("transitions = %d, want 0 (window reset)", fakeRepo.transitions)
	}
}

func TestAutoDisable_ConcurrentRecordErrors(t *testing.T) {
	// Garante thread-safety sob concorrência.
	now := time.Now()
	m, _, _ := newAutoDisableManagerForTest(t, now)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	var wg sync.WaitGroup
	const N = 50
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_ = m.RecordError(ctx, org, key, 1, "deepseek", 429, []byte(`fail`))
		}()
	}
	wg.Wait()
	// Não deve panicar; transições ocorrem conforme contagem exceder threshold.
}

// =============================================================================
// Auditoria de transições (issue #19, RF-03).
// =============================================================================

// TestAutoDisable_AuditsTransitionWithUpstreamMessage valida que a transição
// para auto_disabled gera UM evento de auditoria com a camada do limite e a
// mensagem upstream sanitizada (amostra real do Z.ai).
func TestAutoDisable_AuditsTransitionWithUpstreamMessage(t *testing.T) {
	now := time.Now()
	m, _, _ := newAutoDisableManagerForTest(t, now)
	aud := &fakeAuditor{}
	m.SetTransitionAuditor(aud)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	body := []byte(`{"error":{"code":"1308","message":"Limit of usage is reached for 40 requests in 5 hours, limit will be reset at ` +
		now.Add(2*time.Hour).UTC().Format(time.RFC3339) + `"}}`)
	for i := 0; i < 5; i++ {
		_ = m.RecordError(ctx, org, key, 1, "zai", 429, body)
	}

	if len(aud.events) != 1 {
		t.Fatalf("eventos de auditoria = %d, want 1", len(aud.events))
	}
	if aud.events[0] != AuditActionAutoDisable {
		t.Errorf("action = %q, want %q", aud.events[0], AuditActionAutoDisable)
	}
	meta := aud.metas[0]
	if meta["limit_layer"] != LimitLayerWindow5h {
		t.Errorf("limit_layer = %v, want %q", meta["limit_layer"], LimitLayerWindow5h)
	}
	msg, _ := meta["error_message"].(string)
	if msg == "" || len(msg) > 204 {
		t.Errorf("error_message inválido: %q (len=%d)", msg, len(msg))
	}
	if !strings.Contains(msg, `"code":"1308"`) {
		t.Errorf("error_message deveria conter a mensagem upstream sanitizada: %q", msg)
	}
}

// TestAutoDisable_AuditFailureDoesNotBreakTransition valida que falha do
// auditor é best-effort (a transição já persistida não é afetada).
func TestAutoDisable_AuditFailureDoesNotBreakTransition(t *testing.T) {
	now := time.Now()
	m, fakeRepo, _ := newAutoDisableManagerForTest(t, now)
	m.SetTransitionAuditor(&fakeAuditor{failErr: context.DeadlineExceeded})
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	for i := 0; i < 5; i++ {
		if err := m.RecordError(ctx, org, key, 1, "deepseek", 429, []byte(`fail`)); err != nil {
			t.Fatalf("RecordError[%d]: %v", i, err)
		}
	}
	if fakeRepo.transitions != 1 {
		t.Errorf("transitions = %d, want 1 (audit failure não pode afetar a transição)", fakeRepo.transitions)
	}
}
