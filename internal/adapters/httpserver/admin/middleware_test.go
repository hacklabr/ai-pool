package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Testes de middlewares do admin: CSRF double-submit, session auth, RBAC deny.
// =============================================================================

// newTestApp monta um App com SessionStore in-memory (sem DB) para testar
// middlewares isoladamente.
func newTestApp() *App {
	return &App{
		sessions: nil, // populado abaixo
		pepper:   []byte("test-pepper-32-bytes-aaaaaaaaaaa!"),
		fakeHash: domain.NewFakePasswordHash(),
		cookieCfg: cookieConfig{
			SessionName: "aipp_sess",
			CSRFName:    "aipp_csrf",
			Path:        "/",
			Secure:      false, // dev
			SameSite:    http.SameSiteLaxMode,
			MaxAge:      int(domain.SessionTTLDefault.Seconds()),
		},
		devMode: true,
	}
}

// --- CSRF ---

// TestCSRF_SafeMethodPassa GET/HEAD/OPTIONS não exigem CSRF.
func TestCSRF_SafeMethodPassa(t *testing.T) {
	app := newTestApp()
	called := false
	h := app.csrfMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		called = false
		req := httptest.NewRequest(method, "/api/web/x", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !called {
			t.Errorf("%s deveria passar pelo CSRF sem exigir token", method)
		}
	}
}

// TestCSRF_MutationSemCookieRejeitada POST sem cookie CSRF → 403.
func TestCSRF_MutationSemCookieRejeitada(t *testing.T) {
	app := newTestApp()
	h := app.csrfMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler não deveria ser chamado sem CSRF")
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/web/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestCSRF_MutationSemHeaderRejeitada POST com cookie mas sem header → 403.
func TestCSRF_MutationSemHeaderRejeitada(t *testing.T) {
	app := newTestApp()
	h := app.csrfMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler não deveria ser chamado sem header CSRF")
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/web/x", nil)
	req.AddCookie(&http.Cookie{Name: "aipp_csrf", Value: "tok-123"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestCSRF_MismatchRejeitada cookie != header → 403.
func TestCSRF_MismatchRejeitada(t *testing.T) {
	app := newTestApp()
	h := app.csrfMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("handler não deveria ser chamado com mismatch")
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/web/x", nil)
	req.AddCookie(&http.Cookie{Name: "aipp_csrf", Value: "cookie-val"})
	req.Header.Set("X-CSRF-Token", "header-val") // diferente
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestCSRF_HappyPath cookie == header → passa.
func TestCSRF_HappyPath(t *testing.T) {
	app := newTestApp()
	called := false
	h := app.csrfMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/web/x", nil)
	req.AddCookie(&http.Cookie{Name: "aipp_csrf", Value: "matching-tok"})
	req.Header.Set("X-CSRF-Token", "matching-tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Error("handler deveria ser chamado quando CSRF casa")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestCSRF_SessionCheck Bloqueia mesmo com cookie==header se sessão tem CSRF diferente.
// Camada extra: protege contra vazamento isolado do cookie CSRF.
func TestCSRF_SessionCheck(t *testing.T) {
	app := newTestApp()
	// Sessão populada no context (simulando sessionAuthMiddleware anterior).
	called := false
	h := app.csrfMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	// Handler recebe request COM sessão no context cujo CSRFToken é "real-token".
	// Cookie+header combinam entre si ("matching-tok") MAS são diferentes da sessão.
	req := httptest.NewRequest(http.MethodPost, "/api/web/x", nil)
	req.AddCookie(&http.Cookie{Name: "aipp_csrf", Value: "matching-tok"})
	req.Header.Set("X-CSRF-Token", "matching-tok")
	ctx := context.WithValue(req.Context(), ctxKeySession, &sessionContext{
		CSRFToken: "real-token", // diferente de matching-tok
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if called {
		t.Error("handler NÃO deveria ser chamado quando CSRF != session CSRF")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestConstantTimeEqual_SameLength timing-safe compare.
func TestConstantTimeEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"abc", "ab", false}, // tamanho diferente
		{"", "abc", false},   // vazio
		{"abc", "", false},
		{"abcdef-long-token-value", "abcdef-long-token-value", true},
	}
	for _, c := range cases {
		if got := constantTimeEqual(c.a, c.b); got != c.want {
			t.Errorf("constantTimeEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// --- Session Auth ---

// TestSessionAuth_NoCookieRejeita 401 sem cookie de sessão.
func TestSessionAuth_NoCookieRejeita(t *testing.T) {
	app := newTestApp()
	// Precisamos de um session store real. Criamos store in-memory direto
	// (não importamos o pacote store para evitar ciclo; usamos mock mínimo).
	app.sessions = &mockSessionStore{}
	h := app.sessionAuthMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("handler não deveria ser chamado sem sessão")
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/web/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestSessionAuth_HappyPath sessão válida no cookie → handler chamado + ctx populado.
func TestSessionAuth_HappyPath(t *testing.T) {
	app := newTestApp()
	store := &mockSessionStore{
		sess: &domain.Session{
			ID: "valid", UserID: 1, OrgID: 1, Username: "alice",
			Role: domain.RoleUser, CSRFToken: "csrf-x",
			ExpiresAt: time.Now().Add(time.Hour),
		},
	}
	app.sessions = store

	var got *sessionContext
	h := app.sessionAuthMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = sessionFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/web/x", nil)
	req.AddCookie(&http.Cookie{Name: "aipp_sess", Value: "valid"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got == nil {
		t.Fatal("session não populada no context")
	}
	if got.UserID != 1 || got.Username != "alice" || got.Role != "user" {
		t.Errorf("session context errado: %+v", got)
	}
}

// --- RBAC ---

// TestRequireRole_AdminOnly nega usuário comum em rota admin.
func TestRequireRole_AdminOnly(t *testing.T) {
	app := newTestApp()
	// Simula sessionAuth populando context com role=user.
	setup := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxKeySession, &sessionContext{
			Role: string(domain.RoleUser),
		})
		app.requireRoleMiddleware(domain.RoleAdmin, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			t.Error("handler admin não deveria ser chamado por user")
		})).ServeHTTP(w, r.WithContext(ctx))
	})
	req := httptest.NewRequest(http.MethodGet, "/api/web/admin/x", nil)
	rec := httptest.NewRecorder()
	setup.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestRequireRole_AdminAllowed admin passa.
func TestRequireRole_AdminAllowed(t *testing.T) {
	app := newTestApp()
	called := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxKeySession, &sessionContext{
			Role: string(domain.RoleAdmin),
		})
		app.requireRoleMiddleware(domain.RoleAdmin, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			called = true
		})).ServeHTTP(w, r.WithContext(ctx))
	})
	req := httptest.NewRequest(http.MethodGet, "/api/web/admin/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Error("admin deveria ter acesso")
	}
}

// --- Login (sem DB) ---

// TestLogin_BadJSON body inválido → 400.
func TestLogin_BadJSON(t *testing.T) {
	app := newTestApp()
	app.sessions = &mockSessionStore{}
	req := httptest.NewRequest(http.MethodPost, "/web/login", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	app.handleLogin(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestLogin_EmptyCredentials username/senha vazios → 401 (genérico).
func TestLogin_EmptyCredentials(t *testing.T) {
	app := newTestApp()
	app.sessions = &mockSessionStore{}
	req := httptest.NewRequest(http.MethodPost, "/web/login",
		strings.NewReader(`{"username":"","password":""}`))
	rec := httptest.NewRecorder()
	app.handleLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// mockSessionStore satisfaz domain.SessionStore para testes do admin.
type mockSessionStore struct {
	sess *domain.Session // se nil, todas Get retornam ErrSessionNotFound
}

func (m *mockSessionStore) Create(_ context.Context, s *domain.Session) error { m.sess = s; return nil }
func (m *mockSessionStore) Get(_ context.Context, _ string) (*domain.Session, error) {
	if m.sess == nil {
		return nil, domain.ErrSessionNotFound
	}
	out := *m.sess
	return &out, nil
}
func (m *mockSessionStore) Delete(_ context.Context, _ string) error { m.sess = nil; return nil }
func (m *mockSessionStore) DeleteAllForUser(_ context.Context, _ domain.ID) error {
	m.sess = nil
	return nil
}
