package routing

import (
	"testing"
	"time"
)

// =============================================================================
// ClassifyLimitLayer — amostras REAIS dos provedores (issue #19).
// Z.ai: códigos numéricos no body JSON; Kimi: textos das mensagens.
// =============================================================================

func TestClassifyLimitLayer_Zai(t *testing.T) {
	cases := []struct {
		name       string
		slug       string
		statusCode int
		body       string
		wantLayer  string
		wantReset  string // RFC3339 esperado; vazio = nil
	}{
		{
			name:       "1308 janela 5h (amostra real)",
			slug:       "zai",
			statusCode: 429,
			body:       `{"error":{"code":"1308","message":"Limit of usage is reached for 40 requests in 5 hours, limit will be reset at 2026-08-13T19:00:00Z"}}`,
			wantLayer:  LimitLayerWindow5h,
			wantReset:  "2026-08-13T19:00:00Z",
		},
		{
			name:       "1308 sem unidade → fallback weekly",
			slug:       "z-ai",
			statusCode: 429,
			body:       `{"error":{"code":"1308","message":"Limit of usage reached"}}`,
			wantLayer:  LimitLayerWeekly,
		},
		{
			name:       "1310 semanal por texto de unidade",
			slug:       "zai",
			statusCode: 429,
			body:       `{"error":{"code":"1310","message":"Limit of usage is reached for 240 requests in 7 days, limit will be reset at 2026-08-17T19:00:00Z"}}`,
			wantLayer:  LimitLayerWeekly,
			wantReset:  "2026-08-17T19:00:00Z",
		},
		{
			name:       "1310 mensal por texto de unidade",
			slug:       "zai",
			statusCode: 429,
			body:       `{"error":{"code":"1310","message":"monthly limit reached for this subscription"}}`,
			wantLayer:  LimitLayerMonthly,
		},
		{
			name:       "1113 saldo insuficiente",
			slug:       "zai",
			statusCode: 402,
			body:       `{"error":{"code":"1113","message":"Insufficient balance"}}`,
			wantLayer:  LimitLayerBalance,
		},
		{
			name:       "1309 plano expirado → other",
			slug:       "zai",
			statusCode: 403,
			body:       `{"error":{"code":"1309","message":"subscription expired"}}`,
			wantLayer:  LimitLayerOther,
		},
		{
			name:       "1316 janela 5h",
			slug:       "zai",
			statusCode: 429,
			body:       `{"error":{"code":"1316","message":"5 hours window limit"}}`,
			wantLayer:  LimitLayerWindow5h,
		},
		{
			name:       "1317 janela 5h sem unidade",
			slug:       "zai",
			statusCode: 429,
			body:       `{"error":{"code":"1317","message":"quota reached"}}`,
			wantLayer:  LimitLayerWindow5h,
		},
		{
			name:       "1320 semanal por unidade",
			slug:       "zai",
			statusCode: 429,
			body:       `{"error":{"code":"1320","message":"usage limit, reset in 7 days"}}`,
			wantLayer:  LimitLayerWeekly,
		},
		{
			name:       "1321 sem unidade → other",
			slug:       "zai",
			statusCode: 429,
			body:       `{"error":{"code":"1321","message":"usage limit reached"}}`,
			wantLayer:  LimitLayerOther,
		},
		{
			name:       "next_flush_time extraído como resetAt",
			slug:       "zai",
			statusCode: 429,
			body:       `{"error":{"code":"1308","message":"limit reached"},"next_flush_time":"2026-08-13T21:30:00Z"}`,
			wantLayer:  LimitLayerWeekly,
			wantReset:  "2026-08-13T21:30:00Z",
		},
		{
			name:       "código desconhecido → other",
			slug:       "zai",
			statusCode: 429,
			body:       `{"error":{"code":"9999","message":"mystery"}}`,
			wantLayer:  LimitLayerOther,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layer, resetAt := ClassifyLimitLayer(tc.slug, tc.statusCode, tc.body)
			if layer != tc.wantLayer {
				t.Errorf("layer = %q, want %q", layer, tc.wantLayer)
			}
			if tc.wantReset == "" {
				if resetAt != nil {
					t.Errorf("resetAt = %v, want nil", resetAt)
				}
				return
			}
			if resetAt == nil {
				t.Fatalf("resetAt = nil, want %s", tc.wantReset)
			}
			want, err := time.Parse(time.RFC3339, tc.wantReset)
			if err != nil {
				t.Fatalf("fixture inválida: %v", err)
			}
			if !resetAt.Equal(want) {
				t.Errorf("resetAt = %v, want %v", resetAt, want)
			}
		})
	}
}

func TestClassifyLimitLayer_Kimi(t *testing.T) {
	cases := []struct {
		name       string
		slug       string
		statusCode int
		body       string
		wantLayer  string
	}{
		{
			name:       "janela 5h (amostra real)",
			slug:       "kimi",
			statusCode: 429,
			body:       `You've reached your usage limit for this period. Your quota will be refreshed in the next period.`,
			wantLayer:  LimitLayerWindow5h,
		},
		{
			name:       "mensal (amostra real; contém billing cycle — monthly deve vencer)",
			slug:       "kimi",
			statusCode: 429,
			body:       `You've reached kimi monthly usage limit for this billing cycle.`,
			wantLayer:  LimitLayerMonthly,
		},
		{
			name:       "ciclo de cobrança 403 → semanal (amostra real)",
			slug:       "kimi",
			statusCode: 403,
			body:       `You've reached your usage limit for this billing cycle. Your quota will be refreshed in the next cycle.`,
			wantLayer:  LimitLayerWeekly,
		},
		{
			name:       "tier sem acesso (amostra real)",
			slug:       "moonshot",
			statusCode: 401,
			body:       `Your current subscription does not have access to k3. Upgrade to an Moderato plan or above.`,
			wantLayer:  LimitLayerTier,
		},
		{
			name:       "texto desconhecido → other",
			slug:       "kimi",
			statusCode: 429,
			body:       `something else entirely`,
			wantLayer:  LimitLayerOther,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layer, _ := ClassifyLimitLayer(tc.slug, tc.statusCode, tc.body)
			if layer != tc.wantLayer {
				t.Errorf("layer = %q, want %q", layer, tc.wantLayer)
			}
		})
	}
}

func TestClassifyLimitLayer_UnknownProvider(t *testing.T) {
	layer, resetAt := ClassifyLimitLayer("deepseek", 429, `rate limited, retry at 2026-08-13T19:00:00Z`)
	if layer != LimitLayerOther {
		t.Errorf("layer = %q, want other", layer)
	}
	// resetAt ainda é extraído best-effort.
	if resetAt == nil {
		t.Error("resetAt = nil, want parsed RFC3339")
	}
}

// =============================================================================
// ClassifyErrorClass — de-quebra (b): error_class no UsageEvent.
// =============================================================================

func TestClassifyErrorClass(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	cases := []struct {
		name       string
		statusCode int
		slug       string
		body       string
		want       *string
	}{
		{"429 sem corpo → rate_limit", 429, "deepseek", "", strPtr("rate_limit")},
		{"429 Z.ai 1113 → quota", 429, "zai", `{"error":{"code":"1113"}}`, strPtr("quota")},
		{"429 Kimi período → quota", 429, "kimi", `You've reached your usage limit for this period.`, strPtr("quota")},
		{"401 → auth", 401, "zai", "", strPtr("auth")},
		{"403 → auth", 403, "kimi", "", strPtr("auth")},
		{"500 → 5xx", 500, "zai", "", strPtr("5xx")},
		{"503 → 5xx", 503, "", "", strPtr("5xx")},
		{"200 → nil", 200, "zai", "", nil},
		{"400 → nil (erro do cliente)", 400, "zai", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyErrorClass(tc.statusCode, tc.slug, tc.body)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("got %q, want nil", *got)
			case tc.want != nil && got == nil:
				t.Errorf("got nil, want %q", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Errorf("got %q, want %q", *got, *tc.want)
			}
		})
	}
}
