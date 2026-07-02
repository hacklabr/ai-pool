package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// TestAdminRoutes_RequireAuth verifica que as novas rotas de gestão retornam
// 401 (ou 302 para HTML) quando chamadas sem sessão autenticada.
func TestAdminRoutes_RequireAuth(t *testing.T) {
	a := NewApp(nil, &mockSessionStore{}, nil, true)
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
		{"GET", "/api/web/admin/keys/1", http.StatusUnauthorized},
		{"PATCH", "/api/web/admin/keys/1", http.StatusForbidden},
		{"DELETE", "/api/web/admin/keys/1", http.StatusForbidden},
		{"GET", "/api/web/admin/users/1", http.StatusUnauthorized},
		{"PATCH", "/api/web/admin/users/1", http.StatusForbidden},
		{"DELETE", "/api/web/admin/users/1", http.StatusForbidden},
		{"GET", "/api/web/admin/tokens", http.StatusUnauthorized},
		{"POST", "/api/web/admin/tokens/1/revoke", http.StatusForbidden},
		{"DELETE", "/api/web/admin/tokens/1", http.StatusForbidden},
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

// TestAdminRoutes_RequireAdminRole verifica que um usuário com role=user
// recebe 403 nas rotas admin mesmo autenticado.
func TestAdminRoutes_RequireAdminRole(t *testing.T) {
	store := &mockSessionStore{sess: &domain.Session{
		ID: "test-sess-user", UserID: 2, OrgID: 1, Username: "user",
		Role: domain.RoleUser, CSRFToken: "csrf-token",
	}}
	a := NewApp(nil, store, nil, true)
	h := a.Handler()

	req := httptest.NewRequest("GET", "/api/web/admin/providers", nil)
	req.AddCookie(&http.Cookie{Name: "aipp_sess", Value: "test-sess-user"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin, got %d", rec.Code)
	}
}
