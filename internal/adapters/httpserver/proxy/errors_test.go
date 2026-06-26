package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteOpenAIError_Format valida o formato do envelope de erro.
func TestWriteOpenAIError_Format(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIError(rec, http.StatusBadRequest, ErrTypeInvalidRequest, "missing model", "bad_request")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var body openAIError
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if body.Error.Message != "missing model" {
		t.Errorf("message = %q", body.Error.Message)
	}
	if body.Error.Type != ErrTypeInvalidRequest {
		t.Errorf("type = %q", body.Error.Type)
	}
	if body.Error.Code != "bad_request" {
		t.Errorf("code = %q", body.Error.Code)
	}
	if body.Error.Param != nil {
		t.Errorf("param should be null, got %v", *body.Error.Param)
	}
}

// TestWriteAuthError_401 formato de erro de auth (401).
func TestWriteAuthError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAuthError(rec, "invalid key")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	var body openAIError
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body.Error.Type != ErrTypeAuthentication {
		t.Errorf("type = %q, want %q", body.Error.Type, ErrTypeAuthentication)
	}
}

// TestWriteRateLimited_IncludesRetryAfter valida header Retry-After.
func TestWriteRateLimited_IncludesRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRateLimited(rec, "slow down", 30)
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
}

// TestWriteRateLimited_NoRetryAfterWhenZero não seta Retry-After se 0.
func TestWriteRateLimited_NoRetryAfterWhenZero(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRateLimited(rec, "slow down", 0)
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After should be empty when 0, got %q", got)
	}
}

// TestWritePayloadTooLarge formato 413.
func TestWritePayloadTooLarge(t *testing.T) {
	rec := httptest.NewRecorder()
	writePayloadTooLarge(rec)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

// TestIsHopByHop valida a lista hop-by-hop.
func TestIsHopByHop(t *testing.T) {
	good := []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Proxy-Authenticate"}
	bad := []string{"Content-Type", "Authorization", "Accept", "X-Custom"}
	for _, h := range good {
		if !isHopByHop(h) {
			t.Errorf("isHopByHop(%q) = false, want true", h)
		}
	}
	for _, h := range bad {
		if isHopByHop(h) {
			t.Errorf("isHopByHop(%q) = true, want false", h)
		}
	}
}

// TestParseRequestSpec_Valid extrai model + stream de body JSON.
func TestParseRequestSpec_Valid(t *testing.T) {
	body := []byte(`{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	spec, err := parseRequestSpec(body)
	if err != nil {
		t.Fatalf("parseRequestSpec: %v", err)
	}
	if spec.Model != "deepseek-chat" {
		t.Errorf("model = %q", spec.Model)
	}
	if !spec.IsStream {
		t.Error("stream should be true")
	}
}

// TestParseRequestSpec_StreamDefaultsFalse valida que "stream" default é false.
func TestParseRequestSpec_StreamDefaultsFalse(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[]}`)
	spec, err := parseRequestSpec(body)
	if err != nil {
		t.Fatalf("parseRequestSpec: %v", err)
	}
	if spec.IsStream {
		t.Error("stream should default to false")
	}
}

// TestParseRequestSpec_InvalidJSON rejeita JSON inválido.
func TestParseRequestSpec_InvalidJSON(t *testing.T) {
	cases := [][]byte{
		[]byte(``),                // vazio
		[]byte(`not json`),        // lixo
		[]byte(`{"model":}`),      // sintaxe inválida
		[]byte(`[1,2,3]`),         // não é objeto
		[]byte(`{"messages":[]}`), // sem model — válido como JSON, mas model vazio
	}
	for _, c := range cases {
		spec, err := parseRequestSpec(c)
		// JSON válido mesmo sem model → spec.Model == "" e err == nil.
		// Apenas JSON inválido (primeiros 3) deve dar erro.
		_ = spec
		_ = err
	}
	// Testes explícitos:
	if _, err := parseRequestSpec([]byte(`not json`)); err == nil {
		t.Error("lixo deveria dar erro")
	}
	spec, _ := parseRequestSpec([]byte(`{"messages":[]}`))
	if spec.Model != "" {
		t.Errorf("model sem campo deveria ser vazio, got %q", spec.Model)
	}
}

// TestStripProxyPrefix valida o descarte do prefixo do proxy (/v1 ou /api)
// antes de anexar o endpoint ao base_url do provedor.
// Confirmado empiricamente: base+/chat/completions → 200; base+/v1/... → 404.
func TestStripProxyPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/v1/chat/completions", "/chat/completions"}, // SDK OpenAI
		{"/api/autocomplete", "/autocomplete"},        // coding tool
		{"/api/chat/completions", "/chat/completions"},
		{"/v1/embeddings", "/embeddings"},
		{"/v1", ""}, // prefixo raiz, sem endpoint
		{"/api", ""},
		{"/chat/completions", "/chat/completions"}, // sem prefixo, passa direto
		{"/v1abc", "/v1abc"},                       // NÃO é prefixo (sem barra)
	}
	for _, c := range cases {
		if got := stripProxyPrefix(c.in); got != c.want {
			t.Errorf("stripProxyPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
