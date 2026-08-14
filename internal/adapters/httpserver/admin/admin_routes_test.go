package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// TestAdminRoutes_RequireAuth verifica que as rotas de gestão admin retornam
// 401 (ou 403 para mutações sem CSRF) quando chamadas sem sessão autenticada.
func TestAdminRoutes_RequireAuth(t *testing.T) {
	a := NewApp(nil, &mockSessionStore{}, nil, true, "")
	h := a.Handler()

	tests := []struct {
		method string
		path   string
		want   int
	}{
		{"GET", "/api/web/admin/providers/1", http.StatusUnauthorized},
		{"PATCH", "/api/web/admin/providers/1", http.StatusForbidden},
		{"DELETE", "/api/web/admin/providers/1", http.StatusForbidden},
		{"POST", "/api/web/admin/providers/1/toggle-active", http.StatusForbidden},
		{"GET", "/api/web/admin/models/1", http.StatusUnauthorized},
		{"PATCH", "/api/web/admin/models/1", http.StatusForbidden},
		{"DELETE", "/api/web/admin/models/1", http.StatusForbidden},
		{"POST", "/api/web/admin/models/1/toggle-active", http.StatusForbidden},
		{"POST", "/api/web/admin/models/sync", http.StatusForbidden},
		{"GET", "/api/web/admin/keys/1", http.StatusUnauthorized},
		{"PATCH", "/api/web/admin/keys/1", http.StatusForbidden},
		{"DELETE", "/api/web/admin/keys/1", http.StatusForbidden},
		{"GET", "/api/web/admin/users/1", http.StatusUnauthorized},
		{"PATCH", "/api/web/admin/users/1", http.StatusForbidden},
		{"DELETE", "/api/web/admin/users/1", http.StatusForbidden},
		{"GET", "/api/web/admin/tokens", http.StatusUnauthorized},
		{"POST", "/api/web/admin/tokens/1/revoke", http.StatusForbidden},
		{"DELETE", "/api/web/admin/tokens/1", http.StatusForbidden},
		{"GET", "/api/web/admin/invitations", http.StatusUnauthorized},
		{"POST", "/api/web/admin/invitations", http.StatusForbidden},
		{"PATCH", "/api/web/admin/invitations/1/revoke", http.StatusForbidden},
		{"DELETE", "/api/web/admin/invitations/1", http.StatusForbidden},
		{"GET", "/web/admin/tokens", http.StatusUnauthorized},
	}

	for _, tc := range tests {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s: expected status %d, got %d", tc.method, tc.path, tc.want, rec.Code)
		}
	}
}

// TestInviteRoutes_Public verifica que os endpoints públicos de convite NÃO
// exigem autenticação (GET /invite/{token} retorna o script mesmo sem sessão).
func TestInviteRoutes_Public(t *testing.T) {
	a := NewApp(nil, &mockSessionStore{}, nil, true, "")
	h := a.Handler()

	// GET /invite/{token} deve retornar 200 (script bash) sem auth.
	req := httptest.NewRequest("GET", "/invite/inv-AbCdEf12-testsecret123", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /invite/{token}: expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/x-shellscript; charset=utf-8" {
		t.Errorf("Content-Type: expected text/x-shellscript, got %s", ct)
	}
}

// TestAdminRoutes_RequireAdminRole verifica que um usuário com role=user
// recebe 403 nas rotas admin mesmo autenticado.
func TestAdminRoutes_RequireAdminRole(t *testing.T) {
	store := &mockSessionStore{sess: &domain.Session{
		ID: "test-sess-user", UserID: 2, OrgID: 1, Username: "user",
		Role: domain.RoleUser, CSRFToken: "csrf-token",
	}}
	a := NewApp(nil, store, nil, true, "")
	h := a.Handler()

	req := httptest.NewRequest("GET", "/api/web/admin/providers", nil)
	req.AddCookie(&http.Cookie{Name: "aipp_sess", Value: "test-sess-user"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin on providers, got %d", rec.Code)
	}

	// Convites também exigem admin.
	req2 := httptest.NewRequest("GET", "/api/web/admin/invitations", nil)
	req2.AddCookie(&http.Cookie{Name: "aipp_sess", Value: "test-sess-user"})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin on invitations, got %d", rec2.Code)
	}
}
