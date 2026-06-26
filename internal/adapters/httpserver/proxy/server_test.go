package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rafael/ai-pool/internal/config"
	"github.com/rafael/ai-pool/internal/domain"
	"log/slog"
)

// =============================================================================
// Testes de integração do Proxy Core com mock upstream.
//
// Estes testes exercitam o fluxo completo:
//   - AuthMiddleware (stub que aceita tudo ou nega tudo, conforme configurado).
//   - parseRequestSpec.
//   - KeyResolver (stub retornando provedor mock + URL do upstream de teste).
//   - buildUpstreamRequest + sendUpstream.
//   - handleUpstreamResponse (copy OU streamSSE).
//
// Não usamos DB real. Os stubs são definidos abaixo.
// =============================================================================

// --- Stubs de dependências ---

// acceptAllTokenValidator aceita QUALQUER token; popula user admin.
// Útil para testes que querem focar no fluxo de proxy, não em auth.
type acceptAllTokenValidator struct {
	user *domain.User
}

func (a acceptAllTokenValidator) Validate(_ context.Context, _ string) (*domain.User, *domain.APIToken, error) {
	if a.user == nil {
		a.user = &domain.User{ID: 1, OrgID: domain.DefaultOrganizationID, Username: "tester", Role: domain.RoleUser, Status: domain.UserStatusActive}
	}
	tok := &domain.APIToken{ID: 10, UserID: a.user.ID, OrgID: a.user.OrgID, KeyID: "TESTTEST", Status: domain.APITokenStatusActive}
	return a.user, tok, nil
}

// staticKeyResolver sempre retorna o mesmo provider+key+url (para testes).
type staticKeyResolver struct {
	provider domain.Provider
	key      domain.ProviderKey
	value    []byte
}

func (s staticKeyResolver) ResolveKey(_ context.Context, _ domain.OrganizationID, _ string) (*domain.KeyResolution, error) {
	return &domain.KeyResolution{
		Provider:       s.provider,
		Key:            s.key,
		DecryptedValue: s.value,
	}, nil
}

// --- Helpers ---

func newTestServer(t *testing.T, upstreamURL string) (*Server, *config.Config) {
	t.Helper()
	cfg := &config.Config{
		DevMode:                       true,
		UpstreamOverallTimeout:        5 * time.Second,
		UpstreamFirstByteTimeout:      2 * time.Second,
		UpstreamIdleChunkTimeout:      2 * time.Second,
		UpstreamResponseHeaderTimeout: 2 * time.Second,
		RequestBodyMaxBytes:           1024 * 1024,
	}
	provider := domain.Provider{
		ID:       1,
		OrgID:    domain.DefaultOrganizationID,
		Name:     "TestProvider",
		Slug:     "test",
		BaseURL:  upstreamURL, // já termina sem /
		APISpec:  "openai",
		IsActive: true,
	}
	key := domain.ProviderKey{
		ID:         100,
		ProviderID: 1,
		OrgID:      domain.DefaultOrganizationID,
		Label:      "test-key",
		Status:     domain.KeyStatusEnabled,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(
		cfg, logger,
		acceptAllTokenValidator{},
		staticKeyResolver{provider: provider, key: key, value: []byte("test-secret-key")},
		http.DefaultClient, // para httptest.NewServer (que usa http.DefaultTransport)
	)
	return srv, cfg
}

// --- Testes ---

// TestProxy_NonStreamingChat happy path: cliente manda chat sem stream,
// provedor mock responde com JSON, proxy repassa.
func TestProxy_NonStreamingChat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Valida que recebeu Authorization substituída.
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer test-secret-key") {
			t.Errorf("upstream got wrong auth: %q", auth)
		}
		// Valida User-Agent default (sem override configurado).
		ua := r.Header.Get("User-Agent")
		if ua == "" {
			// Go coloca um default; tudo bem se não setamos override.
		}
		// Valida path: o prefixo /v1 do proxy é DESCARTADO (a versão vive no
		// base_url do provedor); o endpoint /chat/completions é anexado a ele.
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	srv, _ := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-aipp-TESTTEST-"+strings.Repeat("x", domain.SecretLength))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("body não contém resposta esperada: %s", body)
	}
}

// TestProxy_StreamingChat happy path: streaming SSE com flush imediato.
//
// O mock upstream envia 3 chunks SSE; o proxy deve repassar todos.
func TestProxy_StreamingChat(t *testing.T) {
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream sem Flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for _, c := range chunks {
			_, _ = w.Write([]byte(c))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	srv, _ := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-aipp-TESTTEST-"+strings.Repeat("x", domain.SecretLength))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"hel", "lo", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("body não contém %q: %s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// TestProxy_StreamingClientCancel propagação de cancelamento do cliente.
// Quando o cliente desconecta (ctx cancelado), o upstream DEVE parar de receber.
//
// Versão simplificada: validamos apenas que o proxy não trava quando o ctx é
// cancelado. Verificação detalhada de "poupança de tokens no provedor" exigiria
// instrumentar o mock para contar reads após cancelamento (exercício futuro).
func TestProxy_StreamingClientCancel(t *testing.T) {
	upstreamHit := make(chan struct{}, 1)
	upstreamDone := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		upstreamHit <- struct{}{}
		// Fica enviando chunks até o ctx cancelar.
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				upstreamDone <- struct{}{}
				return
			case <-ticker.C:
				_, err := w.Write([]byte("data: {\"choices\":[]}\n\n"))
				if err != nil {
					upstreamDone <- struct{}{}
					return
				}
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	srv, _ := newTestServer(t, upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-aipp-TESTTEST-"+strings.Repeat("x", domain.SecretLength))
	req.Header.Set("Content-Type", "application/json")

	// Executa em goroutine; cancelamos após receber primeiro chunk.
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-upstreamHit:
		// upstream começou a responder — cancela cliente.
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("upstream não foi chamado em 2s")
	}

	// Espera o proxy terminar (deve terminar rápido após cancel).
	select {
	case <-done:
		// OK.
	case <-time.After(3 * time.Second):
		t.Fatal("proxy não terminou 3s após cancel do cliente (leak de goroutine?)")
	}
}

// TestProxy_AuthMissing sem Authorization → 401.
func TestProxy_AuthMissing(t *testing.T) {
	srv, _ := newTestServer(t, "http://unused.example")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestProxy_AuthMalformed Authorization sem Bearer → 401.
func TestProxy_AuthMalformed(t *testing.T) {
	srv, _ := newTestServer(t, "http://unused.example")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Token xyz") // não é "Bearer ..."
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestProxy_BodyTooLarge excedendo max → 413.
func TestProxy_BodyTooLarge(t *testing.T) {
	srv, _ := newTestServer(t, "http://unused.example")
	srv.maxBodyBytes = 100 // força limite baixo p/ teste
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(strings.Repeat("x", 500)))
	req.Header.Set("Authorization", "Bearer sk-aipp-TESTTEST-"+strings.Repeat("x", domain.SecretLength))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

// TestProxy_InvalidJSON body não-JSON → 400.
func TestProxy_InvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t, "http://unused.example")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`not json at all`))
	req.Header.Set("Authorization", "Bearer sk-aipp-TESTTEST-"+strings.Repeat("x", domain.SecretLength))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestProxy_MissingModel JSON válido mas sem "model" → 400.
func TestProxy_MissingModel(t *testing.T) {
	srv, _ := newTestServer(t, "http://unused.example")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-aipp-TESTTEST-"+strings.Repeat("x", domain.SecretLength))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestProxy_HealthzReadyz endpoints de health respondem 200 sem auth.
func TestProxy_HealthzReadyz(t *testing.T) {
	srv, _ := newTestServer(t, "http://unused.example")
	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, rec.Code)
		}
	}
}

// TestProxy_UpstreamErrorRepassado provedor retorna 429 → proxy repassa ao cliente.
// SDKs OpenAI conseguem interpretar o 429 e fazer retry automático.
func TestProxy_UpstreamErrorRepassado(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down"}}`))
	}))
	defer upstream.Close()

	srv, _ := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-aipp-TESTTEST-"+strings.Repeat("x", domain.SecretLength))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (repassado)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "slow down") {
		t.Errorf("body não contém mensagem do provedor: %s", rec.Body.String())
	}
}

// TestProxy_RequestBodyPreserved valida que o body ORIGINAL (bytes) é repassado
// sem re-serialização. Provedor mock recebe exatamente os mesmos bytes.
func TestProxy_RequestBodyPreserved(t *testing.T) {
	// Body propositalmente com espaços e keys em ordem não-canônica — se o proxy
	// re-serializar, o JSON minificado será diferente.
	originalBody := `{  "model" : "deepseek-chat" , "messages" : [ { "role" : "user" , "content" : "hi" } ] }`

	var receivedBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	srv, _ := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(originalBody))
	req.Header.Set("Authorization", "Bearer sk-aipp-TESTTEST-"+strings.Repeat("x", domain.SecretLength))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if receivedBody != originalBody {
		t.Errorf("body NÃO foi preservado!\nwant: %q\ngot:  %q", originalBody, receivedBody)
	}
}

// TestProxy_UserAgentOverride valida override global do provider.
func TestProxy_UserAgentOverride(t *testing.T) {
	var receivedUA string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		DevMode:                       true,
		UpstreamOverallTimeout:        5 * time.Second,
		UpstreamFirstByteTimeout:      2 * time.Second,
		UpstreamIdleChunkTimeout:      2 * time.Second,
		UpstreamResponseHeaderTimeout: 2 * time.Second,
		RequestBodyMaxBytes:           1024 * 1024,
	}
	ua := "MyCustomAgent/1.0"
	provider := domain.Provider{
		ID: 1, OrgID: domain.DefaultOrganizationID,
		Name: "Test", Slug: "test", BaseURL: upstream.URL,
		APISpec: "openai", IsActive: true,
		UserAgentGlobal: &ua,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, logger,
		acceptAllTokenValidator{},
		staticKeyResolver{provider: provider, key: domain.ProviderKey{Status: domain.KeyStatusEnabled}, value: []byte("k")},
		http.DefaultClient)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-aipp-TESTTEST-"+strings.Repeat("x", domain.SecretLength))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if receivedUA != "MyCustomAgent/1.0" {
		t.Errorf("User-Agent = %q, want %q", receivedUA, "MyCustomAgent/1.0")
	}
}
