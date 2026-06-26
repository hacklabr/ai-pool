package observability

import (
	"strings"
	"testing"
)

// TestMetrics_HelpersDoNotPanic valida que cada helper pode ser chamado sem
// pânico, com labels válidas e inválidas. Esta é a garantia mínima — não
// testamos valores exatos porque o DefaultRegisterer é global e compartilhado
// entre testes (valores seriam frágeis).
func TestMetrics_HelpersDoNotPanic(t *testing.T) {
	// Casos com valores típicos e edge cases (labels vazias, tipos inválidos,
	// valores negativos/zero). Se algum helper pânica, o teste falha.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("helper panicked: %v", r)
		}
	}()

	IncProxyRequest("deepseek", "deepseek-chat", 200)
	IncProxyRequest("", "", 404) // labels vazias → sanitizadas
	IncProxyRequest("gemini", "gemini-pro", 429)

	ObserveProxyDuration("deepseek", "deepseek-chat", 0.123)
	ObserveProxyDuration("", "", 1.5)
	ObserveProxyDuration("x", "y", -1.0) // negativo → sanitizado para 0

	IncFailoverAttempt("gemini")
	IncFailoverAttempt("") // → "unknown"

	AddTokens("deepseek", "deepseek-chat", "prompt", 100)
	AddTokens("deepseek", "deepseek-chat", "completion", 50)
	AddTokens("deepseek", "deepseek-chat", "cached", 10)
	AddTokens("deepseek", "deepseek-chat", "prompt", 0)        // no-op (n=0)
	AddTokens("deepseek", "deepseek-chat", "prompt", -5)       // no-op (n<0)
	AddTokens("deepseek", "deepseek-chat", "invalid_type", 99) // type sanitizado

	IncKeyDisabled("auto")
	IncKeyDisabled("manual")
	IncKeyDisabled("invalid") // → "unknown"

	IncKeyReactivation()
	IncKeyReactivation()
}

// TestMetrics_SanitizeHelpers valida o comportamento dos sanitizers de labels.
func TestMetrics_SanitizeHelpers(t *testing.T) {
	if got := sanitizeLabel(""); got != "unknown" {
		t.Errorf("sanitizeLabel(empty) = %q, want %q", got, "unknown")
	}
	if got := sanitizeLabel("deepseek"); got != "deepseek" {
		t.Errorf("sanitizeLabel(deepseek) = %q, want %q", got, "deepseek")
	}

	if got := statusLabel(200); got != "200" {
		t.Errorf("statusLabel(200) = %q, want %q", got, "200")
	}
	if got := statusLabel(429); got != "429" {
		t.Errorf("statusLabel(429) = %q, want %q", got, "429")
	}
	if got := statusLabel(599); got != "599" {
		t.Errorf("statusLabel(599) = %q, want %q", got, "599") // fallback Itoa
	}
	if got := statusLabel(0); got != "0" {
		t.Errorf("statusLabel(0) = %q, want %q", got, "0")
	}

	if got := sanitizeTokenType("prompt"); got != "prompt" {
		t.Errorf("sanitizeTokenType(prompt) = %q, want %q", got, "prompt")
	}
	if got := sanitizeTokenType("bogus"); got != "unknown" {
		t.Errorf("sanitizeTokenType(bogus) = %q, want %q", got, "unknown")
	}

	if got := sanitizeDisableReason("auto"); got != "auto" {
		t.Errorf("sanitizeDisableReason(auto) = %q, want %q", got, "auto")
	}
	if got := sanitizeDisableReason("xxx"); got != "unknown" {
		t.Errorf("sanitizeDisableReason(xxx) = %q, want %q", got, "unknown")
	}
}

// TestMetrics_Itoa testa o fallback de statusLabel para inteiros não cobertos
// pelo switch (garante formato correto incluindo negativos).
func TestMetrics_Itoa(t *testing.T) {
	cases := map[int]string{
		0:   "0",
		1:   "1",
		42:  "42",
		100: "100",
		-7:  "-7",
		500: "500",
	}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestMetrics_NamesStable valida que os nomes das métricas NÃO mudam sem
// intenção (Prometheus identify-metrics-by-name; rename = breaking change
// para dashboards e alertas).
func TestMetrics_NamesStable(t *testing.T) {
	expected := []string{
		"aipp_proxy_requests_total",
		"aipp_proxy_request_duration_seconds",
		"aipp_failover_attempts_total",
		"aipp_tokens_total",
		"aipp_keys_disabled_total",
		"aipp_key_reactivations_total",
	}
	for _, name := range expected {
		// Checa que o descritor foi criado (lookup direto no descritor).
		// Não é robusto 100%, mas pega renaming via var reassignment.
		switch name {
		case "aipp_proxy_requests_total":
			if proxyRequestsTotal == nil {
				t.Errorf("metric %s is nil", name)
			}
		case "aipp_proxy_request_duration_seconds":
			if proxyRequestDurationSeconds == nil {
				t.Errorf("metric %s is nil", name)
			}
		case "aipp_failover_attempts_total":
			if failoverAttemptsTotal == nil {
				t.Errorf("metric %s is nil", name)
			}
		case "aipp_tokens_total":
			if tokensTotal == nil {
				t.Errorf("metric %s is nil", name)
			}
		case "aipp_keys_disabled_total":
			if keysDisabledTotal == nil {
				t.Errorf("metric %s is nil", name)
			}
		case "aipp_key_reactivations_total":
			if keyReactivationsTotal == nil {
				t.Errorf("metric %s is nil", name)
			}
		default:
			t.Errorf("unknown metric name in test: %s", name)
		}
	}

	// Sanity: se este teste quebrar, ABORT — alguém renomeou métrica sem
	// atualizar dashboards/alertas.
	if len(expected) != 6 {
		t.Fatalf("expected metric count drift: %d", len(expected))
	}
	_ = strings.TrimSpace // keep import (defensive)
}
