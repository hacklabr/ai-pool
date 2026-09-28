package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// TestDecideTestAction_cobre os principais status codes do provedor e valida
// o mapeamento → ação de status da chave. É a função PURA central do KeyTester.
func TestDecideTestAction(t *testing.T) {
	cases := []struct {
		name            string
		statusCode      int
		header          http.Header
		body            []byte
		wantNewStatus   string
		wantReason      domain.KeyDisabledReason
		wantPersist     bool
		wantUntilNonNil bool // aplicável apenas quando newStatus == auto_disabled
	}{
		{
			name:            "200 OK reativa",
			statusCode:      200,
			header:          http.Header{},
			body:            nil,
			wantNewStatus:   string(domain.KeyStatusEnabled),
			wantReason:      "",
			wantPersist:     true,
			wantUntilNonNil: false,
		},
		{
			name:            "401 auth_failed sem disabled_until",
			statusCode:      401,
			header:          http.Header{},
			body:            []byte(`{"error":{"message":"invalid api key"}}`),
			wantNewStatus:   string(domain.KeyStatusAutoDisabled),
			wantReason:      domain.ReasonAuthFailed,
			wantPersist:     true,
			wantUntilNonNil: false,
		},
		{
			name:            "403 auth_failed sem disabled_until",
			statusCode:      403,
			header:          http.Header{},
			body:            nil,
			wantNewStatus:   string(domain.KeyStatusAutoDisabled),
			wantReason:      domain.ReasonAuthFailed,
			wantPersist:     true,
			wantUntilNonNil: false,
		},
		{
			// Kimi Code responde 403 com "concurrent request limit" quando outra
			// request está em curso — transitório, não é falha de auth (issue #33).
			name:            "403 concurrent não persiste",
			statusCode:      403,
			header:          http.Header{},
			body:            []byte(`{"error":{"message":"You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again.","type":"access_terminated_error"}}`),
			wantNewStatus:   "",
			wantReason:      "",
			wantPersist:     false,
			wantUntilNonNil: false,
		},
		{
			name:            "429 com Retry-After header (segundos)",
			statusCode:      429,
			header:          http.Header{"Retry-After": []string{"3600"}},
			body:            nil,
			wantNewStatus:   string(domain.KeyStatusAutoDisabled),
			wantReason:      domain.ReasonQuotaExceeded,
			wantPersist:     true,
			wantUntilNonNil: true,
		},
		{
			name:            "429 sem Retry-After → fallback 1h",
			statusCode:      429,
			header:          http.Header{},
			body:            []byte(`{"error":"quota exceeded"}`),
			wantNewStatus:   string(domain.KeyStatusAutoDisabled),
			wantReason:      domain.ReasonQuotaExceeded,
			wantPersist:     true,
			wantUntilNonNil: true,
		},
		{
			name:          "500 não persiste (infra do provedor)",
			statusCode:    500,
			header:        http.Header{},
			body:          []byte(`{"error":"internal"}`),
			wantNewStatus: "",
			wantReason:    "",
			wantPersist:   false,
		},
		{
			name:          "502 não persiste",
			statusCode:    502,
			header:        http.Header{},
			body:          nil,
			wantNewStatus: "",
			wantPersist:   false,
		},
		{
			name:          "503 não persiste",
			statusCode:    503,
			header:        http.Header{},
			body:          nil,
			wantNewStatus: "",
			wantPersist:   false,
		},
		{
			name:          "400 bad request não persiste (erro do body, não da chave)",
			statusCode:    400,
			header:        http.Header{},
			body:          []byte(`{"error":{"message":"invalid model"}}`),
			wantNewStatus: "",
			wantPersist:   false,
		},
		{
			name:          "404 não persiste",
			statusCode:    404,
			header:        http.Header{},
			body:          nil,
			wantNewStatus: "",
			wantPersist:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newStatus, reason, until, persist := decideTestAction(tc.statusCode, tc.header, tc.body)
			if newStatus != tc.wantNewStatus {
				t.Errorf("newStatus = %q, want %q", newStatus, tc.wantNewStatus)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if persist != tc.wantPersist {
				t.Errorf("persist = %v, want %v", persist, tc.wantPersist)
			}
			if tc.wantUntilNonNil && until == nil {
				t.Errorf("expected disabled_until non-nil, got nil")
			}
			if !tc.wantUntilNonNil && until != nil {
				t.Errorf("expected disabled_until nil, got %v", *until)
			}
			// Para 429, disabled_until deve estar no futuro (1h mínimo).
			if tc.wantUntilNonNil && until != nil {
				if !until.After(time.Now()) {
					t.Errorf("disabled_until deve ser futuro, got %v", *until)
				}
			}
		})
	}
}

// TestParseRetryAfter valida a extração de Retry-After (header e body).
func TestParseRetryAfter(t *testing.T) {
	t.Run("header segundos", func(t *testing.T) {
		h := http.Header{"Retry-After": []string{"60"}}
		until := parseRetryAfter(h, nil)
		if until == nil {
			t.Fatal("esperado non-nil")
		}
		// Deve ser ~60s no futuro.
		d := time.Until(*until)
		if d < 50*time.Second || d > 70*time.Second {
			t.Errorf("esperado ~60s, got %v", d)
		}
	})

	t.Run("header HTTP-date", func(t *testing.T) {
		future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
		h := http.Header{"Retry-After": []string{future}}
		until := parseRetryAfter(h, nil)
		if until == nil {
			t.Fatal("esperado non-nil")
		}
	})

	t.Run("header inválido", func(t *testing.T) {
		h := http.Header{"Retry-After": []string{"not-a-date"}}
		until := parseRetryAfter(h, nil)
		if until != nil {
			t.Errorf("esperado nil para header inválido, got %v", *until)
		}
	})

	t.Run("body regex JSON", func(t *testing.T) {
		body := []byte(`{"error":{"retry-after": 120, "message":"rate limit"}}`)
		until := parseRetryAfter(http.Header{}, body)
		if until == nil {
			t.Fatal("esperado non-nil via regex body")
		}
		d := time.Until(*until)
		if d < 110*time.Second || d > 130*time.Second {
			t.Errorf("esperado ~120s, got %v", d)
		}
	})

	t.Run("nenhum", func(t *testing.T) {
		until := parseRetryAfter(http.Header{}, []byte(`{"error":"x"}`))
		if until != nil {
			t.Errorf("esperado nil, got %v", *until)
		}
	})
}

// TestSanitizeHint valida a sanitização de body do provedor.
func TestSanitizeHint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"vazio", "", ""},
		{"newlines colapsados", "line1\nline2\nline3", "line1 line2 line3"},
		{"tabs colapsados", "a\t\t\tb", "a b"},
		{"truncado > 200", strings.Repeat("a", 300), strings.Repeat("a", 200) + "..."},
		{"preserva curto", "short message", "short message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeHint([]byte(tc.in))
			if got != tc.want {
				t.Errorf("sanitizeHint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildTestChatBody valida que o body JSON está bem-formado e contem
// max_tokens=1 + o modelo informado. temperature é omitido de propósito
// (provedor aplica default; Kimi coding só aceita 1 — issue #33).
func TestBuildTestChatBody(t *testing.T) {
	body, err := buildTestChatBody("test-model")
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `"max_tokens":1`) {
		t.Errorf("body não contem max_tokens=1: %s", s)
	}
	if !strings.Contains(s, `"model":"test-model"`) {
		t.Errorf("body não contem o model: %s", s)
	}
	if !strings.Contains(s, `"stream":false`) {
		t.Errorf("body não contem stream=false: %s", s)
	}
	if strings.Contains(s, "temperature") {
		t.Errorf("body não deve conter temperature (default do provedor): %s", s)
	}
}

// TestWipeBytes confirma que wipeBytes zera o slice.
func TestWipeBytes(t *testing.T) {
	b := []byte{1, 2, 3, 4, 5}
	wipeBytes(b)
	for i, v := range b {
		if v != 0 {
			t.Errorf("byte %d não zerado: %d", i, v)
		}
	}
}

// TestIsTransientLimit403 valida a detecção do 403 de limite concorrente
// transitório (Kimi Code: "concurrent request limit", access_terminated_error).
func TestIsTransientLimit403(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"kimi access_terminated_error", `{"error":{"message":"You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again.","type":"access_terminated_error"}}`, true},
		{"concurrent limit curto", `concurrent limit reached`, true},
		{"case-insensitive", `Concurrent Request Limit`, true},
		{"403 de auth comum", `{"error":{"message":"forbidden"}}`, false},
		{"rate limit sem concurrent", `{"error":{"message":"rate limit exceeded"}}`, false},
		{"body vazio", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientLimit403([]byte(tc.body)); got != tc.want {
				t.Errorf("isTransientLimit403(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestFormatTransientLimitMessage valida a mensagem de 403 transitório: deve
// deixar claro que o status da chave NÃO foi alterado (issue #33).
func TestFormatTransientLimitMessage(t *testing.T) {
	msg := formatTransientLimitMessage(569 * time.Millisecond)
	if !strings.Contains(msg, "transitório") {
		t.Errorf("esperava 'transitório' na mensagem: %s", msg)
	}
	if !strings.Contains(msg, "não alterado") {
		t.Errorf("esperava 'não alterado' na mensagem: %s", msg)
	}
	if !strings.Contains(msg, "569ms") {
		t.Errorf("esperava latência na mensagem: %s", msg)
	}
}

// TestFormatProviderMessage valida mensagens da UI.
func TestFormatProviderMessage(t *testing.T) {
	msg := formatProviderMessage(500, 100*time.Millisecond, "internal error")
	if !strings.Contains(msg, "500") {
		t.Errorf("esperava status code na mensagem: %s", msg)
	}
	if !strings.Contains(msg, "internal error") {
		t.Errorf("esperava hint na mensagem: %s", msg)
	}
}

// TestFormatDisabledMessage valida mensagens quando chave é desativada.
func TestFormatDisabledMessage(t *testing.T) {
	until := time.Now().Add(1 * time.Hour)
	msg := formatDisabledMessage(429, domain.ReasonQuotaExceeded, &until, 100*time.Millisecond)
	if !strings.Contains(msg, "429") {
		t.Errorf("esperava 429: %s", msg)
	}
	if !strings.Contains(msg, "quota_exceeded") {
		t.Errorf("esperava reason: %s", msg)
	}
	if !strings.Contains(msg, "reativação em") {
		t.Errorf("esperava data de reativação: %s", msg)
	}
}

// TestRetryAfterHeaderNaResposta simula um provedor que responde 429 com
// Retry-After: 60 e valida que decideTestAction extrai corretamente.
// Este é um teste de integração do fluxo de decisão (sem DB).
func TestRetryAfterHeaderNaResposta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	// Reaproveita o parser num cenário end-to-end via HTTP (valida que o header
	// chega corretamente ao decideTestAction via http.Header).
	resp, err := http.Get(srv.URL) // #nosec G107 -- test server
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	newStatus, reason, until, persist := decideTestAction(resp.StatusCode, resp.Header, nil)
	if newStatus != string(domain.KeyStatusAutoDisabled) {
		t.Errorf("newStatus = %q, want auto_disabled", newStatus)
	}
	if reason != domain.ReasonQuotaExceeded {
		t.Errorf("reason = %q, want quota_exceeded", reason)
	}
	if !persist {
		t.Errorf("esperava persist=true")
	}
	if until == nil {
		t.Fatalf("esperava until non-nil")
	}
	d := time.Until(*until)
	if d < 50*time.Second || d > 70*time.Second {
		t.Errorf("esperava ~60s, got %v", d)
	}
}
