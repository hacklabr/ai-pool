package routing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Testes do FailoverChain — orquestrador stateful.
//
// Estratégia: mock RoutingEngine + mock SendUpstreamFunc permitem testar a
// LÓGICA do chain (classificação, exclusão, backoff, committed, esgotamento)
// sem DB/HTTP real.
// =============================================================================

// --- Mocks ---

// mockRoutingEngine implementa domain.RoutingEngine para testes.
// Retorna chaves em sequência configurada e captura callbacks.
type mockRoutingEngine struct {
	mu sync.Mutex
	// chaves a retornar em sequência (uma por ResolveKeyWithAffinity).
	queue      []*domain.KeyResolution
	resolveIdx int
	// captura:
	triedKeys    []domain.ID
	succeededKey domain.ID
	failedKeys   []failedKeyInfo
}

type failedKeyInfo struct {
	KeyID      domain.ID
	StatusCode int
	BodyLen    int
}

func (m *mockRoutingEngine) ResolveKey(_ context.Context, _ domain.OrganizationID, _ string) (*domain.KeyResolution, error) {
	return nil, errors.New("not used in tests")
}

func (m *mockRoutingEngine) ResolveKeyWithAffinity(
	_ context.Context,
	_ domain.OrganizationID,
	_ domain.ID,
	_ string,
	_ string,
	exclude []domain.ID,
) (*domain.KeyResolution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Captura exclude (para validar que chain excluiu corretamente).
	m.triedKeys = append([]domain.ID{}, exclude...)

	// Pega próxima chave não excluída.
	for m.resolveIdx < len(m.queue) {
		res := m.queue[m.resolveIdx]
		m.resolveIdx++
		if res == nil {
			continue
		}
		// Verifica se está excluída.
		excluded := false
		for _, id := range exclude {
			if id == res.Key.ID {
				excluded = true
				break
			}
		}
		if !excluded {
			return res, nil
		}
	}
	return nil, domain.ErrNoKeysAvailable
}

func (m *mockRoutingEngine) OnKeySucceeded(_ context.Context, _ domain.OrganizationID, _ domain.ID, _ string, keyID domain.ID, _ int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.succeededKey = keyID
	return nil
}

func (m *mockRoutingEngine) OnKeyFailed(_ context.Context, _ domain.OrganizationID, keyID domain.ID, _ domain.ID, _ string, statusCode int, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failedKeys = append(m.failedKeys, failedKeyInfo{
		KeyID: keyID, StatusCode: statusCode, BodyLen: len(body),
	})
	return nil
}

// makeKeyResolution helper para criar KeyResolution de teste.
func makeKeyResolution(keyID int64) *domain.KeyResolution {
	return &domain.KeyResolution{
		Provider:       domain.Provider{ID: 1, Slug: "test", BaseURL: "http://unused", APISpec: "openai"},
		Key:            domain.ProviderKey{ID: domain.ID(keyID), ProviderID: 1, Status: domain.KeyStatusEnabled},
		DecryptedValue: []byte("secret"),
	}
}

// makeChain helper comum.
func makeChain(engine domain.RoutingEngine) *FailoverChain {
	return NewFailoverChain(engine, BackoffConfig{
		Base: 1 * time.Millisecond, // reduz tempo de teste
		Cap:  5 * time.Millisecond,
	}, discardLogger())
}

// --- Testes ---

func TestFailoverChain_SuccessFirstTry(t *testing.T) {
	engine := &mockRoutingEngine{queue: []*domain.KeyResolution{makeKeyResolution(1)}}
	chain := makeChain(engine)

	var sentReqs []*http.Request
	send := func(_ context.Context, req *http.Request) UpstreamOutcome {
		sentReqs = append(sentReqs, req)
		// Simula resposta 200 OK.
		return UpstreamOutcome{Resp: &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     make(http.Header),
		}}
	}

	var handledResp *http.Response
	var handlerCalled bool

	result := chain.Execute(context.Background(), ExecuteOptions{
		OrgID: 1, UserID: 1, Model: "test-model",
		BuildRequest: func(_ context.Context, _ *domain.KeyResolution) (*http.Request, error) {
			return httptest.NewRequest("POST", "/v1/x", nil), nil
		},
		HandleResponse: func(_ context.Context, resp *http.Response, _ *domain.KeyResolution) (bool, error) {
			handledResp = resp
			handlerCalled = true
			return true, nil
		},
	}, send)

	if result.Err != nil {
		t.Fatalf("unexpected err: %v", result.Err)
	}
	if !handlerCalled {
		t.Error("HandleResponse should be called on success")
	}
	if handledResp == nil {
		t.Error("handled resp is nil")
	}
	if result.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", result.Attempts)
	}
	if engine.succeededKey != 1 {
		t.Errorf("OnKeySucceeded not called with key 1; got %v", engine.succeededKey)
	}
	if len(engine.failedKeys) != 0 {
		t.Errorf("should not have failed keys; got %d", len(engine.failedKeys))
	}
}

func TestFailoverChain_FailsOverOn429(t *testing.T) {
	engine := &mockRoutingEngine{queue: []*domain.KeyResolution{
		makeKeyResolution(1),
		makeKeyResolution(2),
	}}
	chain := makeChain(engine)

	sendCount := 0
	send := func(_ context.Context, _ *http.Request) UpstreamOutcome {
		sendCount++
		if sendCount == 1 {
			// Primeira tentativa: 429 → retryable.
			return UpstreamOutcome{Resp: &http.Response{
				StatusCode: 429,
				Body:       io.NopCloser(strings.NewReader(`rate limited`)),
				Header:     make(http.Header),
			}}
		}
		// Segunda: 200 OK.
		return UpstreamOutcome{Resp: &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     make(http.Header),
		}}
	}

	result := chain.Execute(context.Background(), ExecuteOptions{
		OrgID: 1, UserID: 1, Model: "test",
		BuildRequest: func(_ context.Context, _ *domain.KeyResolution) (*http.Request, error) {
			return httptest.NewRequest("POST", "/v1/x", nil), nil
		},
		HandleResponse: func(_ context.Context, _ *http.Response, _ *domain.KeyResolution) (bool, error) {
			return true, nil
		},
	}, send)

	if result.Err != nil {
		t.Fatalf("err: %v", result.Err)
	}
	if result.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (failover)", result.Attempts)
	}
	if len(engine.failedKeys) != 1 {
		t.Errorf("should record 1 failed key; got %d", len(engine.failedKeys))
	}
	if engine.failedKeys[0].KeyID != 1 {
		t.Errorf("failed key should be 1; got %v", engine.failedKeys[0].KeyID)
	}
	if engine.succeededKey != 2 {
		t.Errorf("succeeded key should be 2; got %v", engine.succeededKey)
	}
}

func TestFailoverChain_ExhaustsAllKeys(t *testing.T) {
	engine := &mockRoutingEngine{queue: []*domain.KeyResolution{
		makeKeyResolution(1),
		makeKeyResolution(2),
		makeKeyResolution(3),
	}}
	chain := makeChain(engine)

	send := func(_ context.Context, _ *http.Request) UpstreamOutcome {
		// Sempre 429.
		return UpstreamOutcome{Resp: &http.Response{
			StatusCode: 429,
			Body:       io.NopCloser(strings.NewReader(`rate limited`)),
			Header:     make(http.Header),
		}}
	}

	result := chain.Execute(context.Background(), ExecuteOptions{
		OrgID: 1, UserID: 1, Model: "test",
		BuildRequest: func(_ context.Context, _ *domain.KeyResolution) (*http.Request, error) {
			return httptest.NewRequest("POST", "/v1/x", nil), nil
		},
		HandleResponse: func(_ context.Context, _ *http.Response, _ *domain.KeyResolution) (bool, error) {
			t.Error("HandleResponse should NOT be called when all keys fail")
			return false, nil
		},
	}, send)

	if !errors.Is(result.Err, ErrAllKeysExhausted) {
		t.Errorf("err = %v, want ErrAllKeysExhausted", result.Err)
	}
	if result.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", result.Attempts)
	}
	if len(engine.failedKeys) != 3 {
		t.Errorf("should record 3 failures; got %d", len(engine.failedKeys))
	}
	if result.Committed {
		t.Error("should NOT be committed (no success)")
	}
}

func TestFailoverChain_ClientErrorDoesNotFailOver(t *testing.T) {
	// 400 bad request → NÃO dispara failover. Resposta é repassada ao cliente.
	engine := &mockRoutingEngine{queue: []*domain.KeyResolution{
		makeKeyResolution(1),
		makeKeyResolution(2), // nunca deveria ser usado
	}}
	chain := makeChain(engine)

	send := func(_ context.Context, _ *http.Request) UpstreamOutcome {
		return UpstreamOutcome{Resp: &http.Response{
			StatusCode: 400,
			Body:       io.NopCloser(strings.NewReader(`{"error":"bad request"}`)),
			Header:     make(http.Header),
		}}
	}

	result := chain.Execute(context.Background(), ExecuteOptions{
		OrgID: 1, UserID: 1, Model: "test",
		BuildRequest: func(_ context.Context, _ *domain.KeyResolution) (*http.Request, error) {
			return httptest.NewRequest("POST", "/v1/x", nil), nil
		},
		HandleResponse: func(_ context.Context, _ *http.Response, _ *domain.KeyResolution) (bool, error) {
			return true, nil
		},
	}, send)

	if result.Err != nil {
		t.Errorf("err = %v, want nil (client error repassed)", result.Err)
	}
	if result.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no failover on 400)", result.Attempts)
	}
	if !result.Committed {
		t.Error("should be committed (client error response)")
	}
	if len(engine.failedKeys) != 0 {
		t.Errorf("should not record failure for client error; got %d", len(engine.failedKeys))
	}
}

func TestFailoverChain_NetworkErrorIsRetryable(t *testing.T) {
	engine := &mockRoutingEngine{queue: []*domain.KeyResolution{
		makeKeyResolution(1),
		makeKeyResolution(2),
	}}
	chain := makeChain(engine)

	sendCount := 0
	send := func(_ context.Context, _ *http.Request) UpstreamOutcome {
		sendCount++
		if sendCount == 1 {
			return UpstreamOutcome{Resp: nil, Err: errors.New("connection refused")}
		}
		return UpstreamOutcome{Resp: &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     make(http.Header),
		}}
	}

	result := chain.Execute(context.Background(), ExecuteOptions{
		OrgID: 1, UserID: 1, Model: "test",
		BuildRequest: func(_ context.Context, _ *domain.KeyResolution) (*http.Request, error) {
			return httptest.NewRequest("POST", "/v1/x", nil), nil
		},
		HandleResponse: func(_ context.Context, _ *http.Response, _ *domain.KeyResolution) (bool, error) {
			return true, nil
		},
	}, send)

	if result.Err != nil {
		t.Fatalf("err: %v", result.Err)
	}
	if result.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", result.Attempts)
	}
	if len(engine.failedKeys) != 1 {
		t.Errorf("should record 1 failure (network); got %d", len(engine.failedKeys))
	}
	if engine.failedKeys[0].StatusCode != 0 {
		t.Errorf("network failure statusCode = %d, want 0", engine.failedKeys[0].StatusCode)
	}
}

func TestFailoverChain_RespectsContextCancellation(t *testing.T) {
	engine := &mockRoutingEngine{queue: []*domain.KeyResolution{
		makeKeyResolution(1),
		makeKeyResolution(2),
	}}
	chain := makeChain(engine)

	// Backoff grande para garantir que o cancelamento ocorre durante o sleep.
	chain.backoff = BackoffConfig{Base: 1 * time.Second, Cap: 5 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())

	sendCount := 0
	send := func(_ context.Context, _ *http.Request) UpstreamOutcome {
		sendCount++
		// Primeira tentativa: 429.
		cancel() // cancela contexto durante a primeira resposta
		return UpstreamOutcome{Resp: &http.Response{
			StatusCode: 429,
			Body:       io.NopCloser(strings.NewReader(`rate limited`)),
			Header:     make(http.Header),
		}}
	}

	result := chain.Execute(ctx, ExecuteOptions{
		OrgID: 1, UserID: 1, Model: "test",
		BuildRequest: func(_ context.Context, _ *domain.KeyResolution) (*http.Request, error) {
			return httptest.NewRequest("POST", "/v1/x", nil), nil
		},
		HandleResponse: func(_ context.Context, _ *http.Response, _ *domain.KeyResolution) (bool, error) {
			return true, nil
		},
	}, send)

	if result.Err == nil {
		t.Error("expected err from cancelled context")
	}
	if !errors.Is(result.Err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", result.Err)
	}
}
