package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/modelsync"
	webassets "github.com/hacklabr/ai-pool/web"
)

// =============================================================================
// Testes do sync sob demanda (issue #25): POST /api/web/admin/models/sync.
// =============================================================================

// fakeModelSyncRunner stub da porta ModelSyncRunner para testes.
type fakeModelSyncRunner struct {
	report *modelsync.Report
	err    error
}

func (f *fakeModelSyncRunner) SyncAll(_ context.Context) (*modelsync.Report, error) {
	return f.report, f.err
}

// newSyncTestApp monta um App autenticável como admin (sem DB — o handler de
// sync não toca o pool) e retorna o handler completo com o runner injetado.
func newSyncTestApp(runner ModelSyncRunner) http.Handler {
	store := &mockSessionStore{sess: &domain.Session{
		ID: "sess-admin", UserID: 1, OrgID: 1, Username: "admin",
		Role: domain.RoleAdmin, CSRFToken: "csrf-test",
	}}
	a := NewApp(nil, store, nil, true, "")
	if runner != nil {
		a.SetModelSyncRunner(runner)
	}
	return a.Handler()
}

// newAdminPostRequest monta um POST autenticado (cookie de sessão + CSRF
// double-submit: cookie + header com o mesmo valor da sessão).
func newAdminPostRequest(path string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.AddCookie(&http.Cookie{Name: "aipp_sess", Value: "sess-admin"})
	req.AddCookie(&http.Cookie{Name: "aipp_csrf", Value: "csrf-test"})
	req.Header.Set("X-CSRF-Token", "csrf-test")
	return req
}

// TestSyncModelsRoute_SemRunner_503 valida a resposta amigável quando o
// runner não foi injetado (ex.: dev sem DB).
func TestSyncModelsRoute_SemRunner_503(t *testing.T) {
	h := newSyncTestApp(nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newAdminPostRequest("/api/web/admin/models/sync"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("resposta não é JSON do envelope padrão: %v", err)
	}
	if body.Error.Code != "not_configured" {
		t.Errorf("error.code = %q, want not_configured", body.Error.Code)
	}
}

// TestSyncModelsRoute_ComReport_200 valida o DTO de resposta: por provedor
// {slug, added, reactivated, deactivated, skipped, skip_reason} + totais.
func TestSyncModelsRoute_ComReport_200(t *testing.T) {
	runner := &fakeModelSyncRunner{report: &modelsync.Report{
		Providers: []modelsync.ProviderReport{
			{ProviderSlug: "acme", Added: []string{"m1", "m2"}, Reactivated: []string{"m3"}},
			{ProviderSlug: "broken", Skipped: true, SkipReason: "no usable key"},
		},
	}}
	h := newSyncTestApp(runner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newAdminPostRequest("/api/web/admin/models/sync"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp syncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON inválido: %v", err)
	}
	if len(resp.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(resp.Providers))
	}
	acme := resp.Providers[0]
	if acme.Slug != "acme" || acme.Added != 2 || acme.Reactivated != 1 || acme.Deactivated != 0 || acme.Skipped {
		t.Errorf("provider acme inesperado: %+v", acme)
	}
	broken := resp.Providers[1]
	if !broken.Skipped || broken.SkipReason != "no usable key" {
		t.Errorf("provider broken inesperado: %+v", broken)
	}
	want := syncSummary{Providers: 2, Added: 2, Reactivated: 1, Deactivated: 0, Skipped: 1}
	if resp.Summary != want {
		t.Errorf("summary = %+v, want %+v", resp.Summary, want)
	}
}

// TestSyncModelsRoute_ErroEstrutural_500 valida que erro do SyncAll vira 500
// no envelope padrão (falha estrutural de DB).
func TestSyncModelsRoute_ErroEstrutural_500(t *testing.T) {
	runner := &fakeModelSyncRunner{err: errors.New("db down")}
	h := newSyncTestApp(runner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newAdminPostRequest("/api/web/admin/models/sync"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model sync failed") {
		t.Errorf("esperava mensagem de erro do envelope; body=%s", rec.Body.String())
	}
}

// TestSyncModelsRoute_ReportVazio_200 valida rodada sem provedores ativos
// (resposta 200 com providers=[] e summary zerada — não é erro).
func TestSyncModelsRoute_ReportVazio_200(t *testing.T) {
	runner := &fakeModelSyncRunner{report: &modelsync.Report{}}
	h := newSyncTestApp(runner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newAdminPostRequest("/api/web/admin/models/sync"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp syncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON inválido: %v", err)
	}
	if resp.Providers == nil || len(resp.Providers) != 0 {
		t.Errorf("esperava providers=[] (não nil) no JSON; got %+v", resp.Providers)
	}
	if (resp.Summary != syncSummary{}) {
		t.Errorf("esperava summary zerada; got %+v", resp.Summary)
	}
}

// TestSyncModelsRoute_RequerAdminRole valida que user comum recebe 403.
func TestSyncModelsRoute_RequerAdminRole(t *testing.T) {
	store := &mockSessionStore{sess: &domain.Session{
		ID: "sess-user", UserID: 2, OrgID: 1, Username: "user",
		Role: domain.RoleUser, CSRFToken: "csrf-test",
	}}
	a := NewApp(nil, store, nil, true, "")
	h := a.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newAdminPostRequest("/api/web/admin/models/sync"))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestTemplateRenderer_RenderModels valida que a página de modelos renderiza
// o botão "Sincronizar agora" apontando para a rota de sync (issue #25).
// Segue o padrão de TestTemplateRenderer_RenderDashboard.
func TestTemplateRenderer_RenderModels(t *testing.T) {
	r, err := NewTemplateRenderer(webassets.TemplatesFS())
	if err != nil {
		t.Fatalf("NewTemplateRenderer failed: %v", err)
	}

	sess := &sessionContext{
		UserID: 1, OrgID: 1, Username: "admin", Role: "admin",
		CSRFToken: "test-csrf",
	}
	data := pageData{
		Title:     "Modelos",
		ActiveNav: "models",
		User:      sess,
		CSRFToken: "test-csrf",
		// Content espelha o shape usado por handlePageAdminModels.
		Content: struct {
			Models    []modelListItem
			Providers []providerListItem
		}{},
	}

	var buf strings.Builder
	if err := r.pages["pages/models.html"].ExecuteTemplate(&buf, "layouts/base.html", data); err != nil {
		t.Fatalf("render models failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Sincronizar agora",
		`hx-post="/api/web/admin/models/sync"`,
		`hx-disabled-elt="this"`,
		"+ Novo modelo", // header intacto
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render models: missing %q in output", want)
		}
	}
}
