package routing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// fakeQuotaRecorder captura chamadas de RecordCeilingHit para asserção
// (issue #20, RF-05).
type fakeQuotaRecorder struct {
	mu      sync.Mutex
	hits    []recordedHit
	failErr error
}

// recordedHit é uma chamada capturada do recorder.
type recordedHit struct {
	orgID domain.OrganizationID
	keyID domain.ID
	layer string
	at    time.Time
	msg   string
}

func (f *fakeQuotaRecorder) RecordCeilingHit(
	_ context.Context,
	orgID domain.OrganizationID,
	keyID domain.ID,
	layer string,
	disabledAt time.Time,
	sanitizedMessage string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	f.hits = append(f.hits, recordedHit{orgID, keyID, layer, disabledAt, sanitizedMessage})
	return nil
}

// TestAutoDisable_RecordsQuotaMeasurementOnCeilingHit valida que a
// desativação por teto de janela do plano (window_5h no erro Z.ai 1308 com
// texto "in 5 hours") dispara a medição empírica com a mensagem sanitizada.
func TestAutoDisable_RecordsQuotaMeasurementOnCeilingHit(t *testing.T) {
	now := time.Now()
	m, _, _ := newAutoDisableManagerForTest(t, now)
	rec := &fakeQuotaRecorder{}
	m.SetQuotaMeasurementRecorder(rec)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	body := []byte(`{"error":{"code":"1308","message":"Limit of usage is reached for 40 requests in 5 hours, limit will be reset at ` +
		now.Add(2*time.Hour).UTC().Format(time.RFC3339) + `"}}`)
	for i := 0; i < 5; i++ {
		_ = m.RecordError(ctx, org, key, 1, "zai", 429, body)
	}

	if len(rec.hits) != 1 {
		t.Fatalf("medições registradas = %d, want 1", len(rec.hits))
	}
	h := rec.hits[0]
	if h.keyID != key || h.orgID != org {
		t.Errorf("hit org/key = %v/%v, want %v/%v", h.orgID, h.keyID, org, key)
	}
	if h.layer != LimitLayerWindow5h {
		t.Errorf("layer = %q, want %q", h.layer, LimitLayerWindow5h)
	}
	if h.msg == "" || len(h.msg) > 204 {
		t.Errorf("mensagem sanitizada inválida: %q (len=%d)", h.msg, len(h.msg))
	}
	if !h.at.Equal(now) {
		t.Errorf("disabledAt = %v, want %v (instante do disable)", h.at, now)
	}
}

// TestAutoDisable_NoQuotaMeasurementOnNonCeilingLayers valida que camadas
// que NÃO são teto de janela (balance/tier/other) não geram medição.
func TestAutoDisable_NoQuotaMeasurementOnNonCeilingLayers(t *testing.T) {
	now := time.Now()
	m, _, _ := newAutoDisableManagerForTest(t, now)
	rec := &fakeQuotaRecorder{}
	m.SetQuotaMeasurementRecorder(rec)
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	// Z.ai 1113 → balance (saldo insuficiente — não é teto de janela).
	body := []byte(`{"error":{"code":"1113","message":"insufficient balance"}}`)
	for i := 0; i < 5; i++ {
		_ = m.RecordError(ctx, org, key, 1, "zai", 429, body)
	}
	if len(rec.hits) != 0 {
		t.Errorf("medições = %d, want 0 (balance não é teto de janela)", len(rec.hits))
	}
}

// TestAutoDisable_QuotaMeasurementFailureDoesNotBreakDisable valida que a
// calibração é best-effort: falha do recorder NUNCA afeta a transição.
func TestAutoDisable_QuotaMeasurementFailureDoesNotBreakDisable(t *testing.T) {
	now := time.Now()
	m, fakeRepo, _ := newAutoDisableManagerForTest(t, now)
	m.SetQuotaMeasurementRecorder(&fakeQuotaRecorder{failErr: errors.New("db down")})
	ctx := context.Background()
	const org domain.OrganizationID = 1
	const key domain.ID = 100

	body := []byte(`{"error":{"code":"1308","message":"monthly limit reached"}}`)
	for i := 0; i < 5; i++ {
		if err := m.RecordError(ctx, org, key, 1, "zai", 429, body); err != nil {
			t.Fatalf("RecordError[%d]: %v (falha da medição não pode propagar)", i, err)
		}
	}
	if fakeRepo.transitions != 1 {
		t.Errorf("transitions = %d, want 1 (medição falhou mas disable persistiu)", fakeRepo.transitions)
	}
}

// TestAutoDisable_NilQuotaRecorderIsSafe valida o nil-safe do recorder.
func TestAutoDisable_NilQuotaRecorderIsSafe(t *testing.T) {
	now := time.Now()
	m, fakeRepo, _ := newAutoDisableManagerForTest(t, now)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := m.RecordError(ctx, 1, 100, 1, "kimi", 429, []byte(`monthly usage limit for this billing cycle`)); err != nil {
			t.Fatalf("RecordError[%d]: %v", i, err)
		}
	}
	if fakeRepo.transitions != 1 {
		t.Errorf("transitions = %d, want 1", fakeRepo.transitions)
	}
}
