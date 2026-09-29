package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// =============================================================================
// Testes do painel de Estatísticas de Utilização (/web/admin/usage).
//
// Cobre: parse/normalização de filtros (defaults, presets, clamp de 90d,
// valores inválidos ignorados) e guarda-corpes do handler JSON (401 sem
// sessão, 503 sem repo). Queries de dados reais dependem de DB — cobertas
// pela integração, não aqui.
// =============================================================================

// --- Parse de filtros --------------------------------------------------------

// TestParseUsageStatsFilter_Defaults Sem params → janela default 7d, sem
// filtros opcionais.
func TestParseUsageStatsFilter_Defaults(t *testing.T) {
	before := time.Now()
	r := httptest.NewRequest(http.MethodGet, "/web/admin/usage", nil)
	f := parseUsageStatsFilter(r)
	if got := f.To.Sub(before); got < -10*time.Second || got > 10*time.Second {
		t.Errorf("to deveria ser ~agora, diff = %v", got)
	}
	if want := 7 * 24 * time.Hour; f.To.Sub(f.From) > want+10*time.Second || f.To.Sub(f.From) < want-10*time.Second {
		t.Errorf("janela default = %v, want ~%v", f.To.Sub(f.From), want)
	}
	if f.UserID != nil || f.KeyID != nil || f.ProviderID != nil || f.Model != "" {
		t.Errorf("filtros opcionais deveriam estar ausentes: %+v", f)
	}
}

// TestParseUsageStatsFilter_Presets Presets derivam a janela; preset inválido
// cai no default.
func TestParseUsageStatsFilter_Presets(t *testing.T) {
	tests := []struct {
		preset string
		want   time.Duration
	}{
		{"24h", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"30d", 30 * 24 * time.Hour},
		{"hack", 7 * 24 * time.Hour}, // inválido → default
	}
	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodGet, "/web/admin/usage?preset="+tc.preset, nil)
		f := parseUsageStatsFilter(r)
		if got := f.To.Sub(f.From); got < tc.want-10*time.Second || got > tc.want+10*time.Second {
			t.Errorf("preset %q: janela = %v, want ~%v", tc.preset, got, tc.want)
		}
	}
}

// TestParseUsageStatsFilter_CustomDates Datas via input date: "to" é inclusivo
// (estende até o fim do dia).
func TestParseUsageStatsFilter_CustomDates(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/web/admin/usage?from=2026-09-01&to=2026-09-10", nil)
	f := parseUsageStatsFilter(r)
	if got := f.From.Format("2006-01-02"); got != "2026-09-01" {
		t.Errorf("from = %s, want 2026-09-01", got)
	}
	if got := f.To.Format("2006-01-02"); got != "2026-09-10" {
		t.Errorf("to = %s, want 2026-09-10 (inclusivo)", got)
	}
	if f.To.Hour() != 23 || f.To.Minute() != 59 {
		t.Errorf("to deveria ser fim do dia, got %s", f.To.Format("15:04:05"))
	}
}

// TestParseUsageStatsFilter_InvertedDates Guarda from < to: datas invertidas
// caem no default de 7d.
func TestParseUsageStatsFilter_InvertedDates(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/web/admin/usage?from=2026-09-10&to=2026-09-01", nil)
	f := parseUsageStatsFilter(r)
	if want := 7 * 24 * time.Hour; f.To.Sub(f.From) > want+10*time.Second {
		t.Errorf("datas invertidas deveriam cair no default 7d, got %v", f.To.Sub(f.From))
	}
}

// TestParseUsageStatsFilter_Clamp90d Janela maior que a retenção dos logs é
// cortada para 90 dias.
func TestParseUsageStatsFilter_Clamp90d(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/web/admin/usage?from=2026-01-01&to=2026-09-28", nil)
	f := parseUsageStatsFilter(r)
	if max := usageStatsMaxWindow; f.To.Sub(f.From) > max {
		t.Errorf("janela = %v, deveria estar clampada em %v", f.To.Sub(f.From), max)
	}
}

// TestParseUsageStatsFilter_InvalidIDs IDs inválidos/negados/zero viram nil
// (sem filtro), nunca erro.
func TestParseUsageStatsFilter_InvalidIDs(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/web/admin/usage?user_id=abc&key_id=-3&provider_id=0", nil)
	f := parseUsageStatsFilter(r)
	if f.UserID != nil || f.KeyID != nil || f.ProviderID != nil {
		t.Errorf("IDs inválidos deveriam ser ignorados: %+v", f)
	}

	r = httptest.NewRequest(http.MethodGet, "/web/admin/usage?user_id=7&key_id=9", nil)
	f = parseUsageStatsFilter(r)
	if f.UserID == nil || *f.UserID != 7 {
		t.Errorf("user_id=7 deveria ser mantido, got %+v", f.UserID)
	}
	if f.KeyID == nil || *f.KeyID != 9 {
		t.Errorf("key_id=9 deveria ser mantido, got %+v", f.KeyID)
	}
}

// TestParseUsageStatsFilter_ModelCap Model com comprimento excessivo é cortado
// (defesa; valor sempre vai como placeholder, nunca vira SQL).
func TestParseUsageStatsFilter_ModelCap(t *testing.T) {
	long := strings.Repeat("m", 300)
	r := httptest.NewRequest(http.MethodGet, "/web/admin/usage?model="+long, nil)
	f := parseUsageStatsFilter(r)
	if len(f.Model) != 128 {
		t.Errorf("model deveria ser cortado em 128, got %d", len(f.Model))
	}
}

// --- Guardas do handler JSON -------------------------------------------------

// requestWithSession injeta uma sessão admin no contexto (mesmo mecanismo da
// sessionAuthMiddleware).
func requestWithSession(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	ctx := context.WithValue(r.Context(), ctxKeySession, &sessionContext{
		SessionID: "sess-test", CSRFToken: "tok", UserID: 1, OrgID: 1,
		Username: "admin", Role: "admin",
	})
	return r.WithContext(ctx)
}

// TestUsageStatsData_Unauthenticated JSON sem sessão → 401.
func TestUsageStatsData_Unauthenticated(t *testing.T) {
	a := NewApp(nil, &mockSessionStore{}, nil, true, "")
	req := httptest.NewRequest(http.MethodGet, "/api/web/admin/usage/stats", nil)
	rec := httptest.NewRecorder()
	a.handleAdminUsageStatsData(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestUsageStatsData_RepoNil Sessão válida mas usageRepo ausente → 503
// graceful (degradação, não panic). Usa App zerado: NewApp sempre inicializa
// usageRepo (com pool possivelmente nil), então só o zero-value exercita o
// caminho de degradação.
func TestUsageStatsData_RepoNil(t *testing.T) {
	a := &App{}
	rec := httptest.NewRecorder()
	a.handleAdminUsageStatsData(rec, requestWithSession(http.MethodGet, "/api/web/admin/usage/stats"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// --- Rotas -------------------------------------------------------------------

// TestUsageStatsRoutes_RequireAuth As rotas do painel exigem sessão (RBAC do
// grupo admin aplicado em admin.go).
func TestUsageStatsRoutes_RequireAuth(t *testing.T) {
	a := NewApp(nil, &mockSessionStore{}, nil, true, "")
	h := a.Handler()
	tests := []struct {
		method, path string
		want         int
	}{
		{"GET", "/web/admin/usage", http.StatusUnauthorized},
		{"GET", "/api/web/admin/usage/stats", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s: expected %d, got %d", tc.method, tc.path, tc.want, rec.Code)
		}
	}
}
