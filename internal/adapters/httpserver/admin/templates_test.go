package admin

import (
	"strings"
	"testing"

	webassets "github.com/hacklabr/ai-pool/web"
)

// TestTemplateRenderer_LoadFromEmbedFS valida que TODOS os templates do
// embed.FS do pacote web carregam sem erro de parse. Isto captura problemas
// de sintaxe Go template, defines faltando, etc., antes de runtime.
//
// Não testa RENDERIZAÇÃO (output HTML) — apenas que o parse não falha.
// Renderização seria testada com golden files (futuro).
func TestTemplateRenderer_LoadFromEmbedFS(t *testing.T) {
	r, err := NewTemplateRenderer(webassets.TemplatesFS())
	if err != nil {
		t.Fatalf("NewTemplateRenderer failed: %v", err)
	}
	// Verifica que o conjunto esperado de páginas carregou.
	expectedPages := []string{
		"pages/login.html",
		"pages/dashboard.html",
		"pages/users.html",
		"pages/user_detail.html",
		"pages/providers.html",
		"pages/models.html",
		"pages/keys.html",
		"pages/my_tokens.html",
		"pages/my_usage.html",
		"pages/audit.html",
		"pages/tokens.html",
		"pages/usage_stats.html",
	}
	for _, p := range expectedPages {
		if _, ok := r.pages[p]; !ok {
			t.Errorf("expected page %q in renderer cache; not found", p)
		}
	}
	if len(r.pages) != len(expectedPages) {
		t.Errorf("expected %d pages, got %d (extras or missing)", len(expectedPages), len(r.pages))
	}
}

// TestTemplateRenderer_RenderLogin valida que a página de login renderiza
// sem erro (incluindo o layout base + partials). Verifica que o output
// contém marcações esperadas (title, form).
func TestTemplateRenderer_RenderLogin(t *testing.T) {
	r, err := NewTemplateRenderer(webassets.TemplatesFS())
	if err != nil {
		t.Fatalf("NewTemplateRenderer failed: %v", err)
	}

	var buf strings.Builder
	data := pageData{Title: "Test Login", ActiveNav: "login"}
	if err := r.pages["pages/login.html"].ExecuteTemplate(&buf, "layouts/base.html", data); err != nil {
		t.Fatalf("render login failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"<!doctype html>",
		"AI Provider Pool",
		"Entrar",
		`name="username"`,
		`name="password"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render login: missing %q in output", want)
		}
	}
}

// TestTemplateRenderer_RenderDashboard valida renderização do dashboard
// com dados fictícios (simula o pageData completo).
func TestTemplateRenderer_RenderDashboard(t *testing.T) {
	r, err := NewTemplateRenderer(webassets.TemplatesFS())
	if err != nil {
		t.Fatalf("NewTemplateRenderer failed: %v", err)
	}

	// Mock de sessionContext (não precisa de DB real).
	sess := &sessionContext{
		UserID: 1, OrgID: 1, Username: "admin", Role: "admin",
		CSRFToken: "test-csrf",
	}
	data := pageData{
		Title:     "Dashboard",
		ActiveNav: "dashboard",
		User:      sess,
		CSRFToken: "test-csrf",
	}

	var buf strings.Builder
	if err := r.pages["pages/dashboard.html"].ExecuteTemplate(&buf, "layouts/base.html", data); err != nil {
		t.Fatalf("render dashboard failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Dashboard",
		"main.css", // CSS link presente
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render dashboard: missing %q in output", want)
		}
	}
}
