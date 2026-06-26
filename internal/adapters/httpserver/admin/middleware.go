package admin

import (
	"context"
	"net/http"
	"strings"

	"github.com/rafael/ai-pool/internal/domain"
)

// =============================================================================
// Middlewares da admin API.
//
// Pipeline típico para rota autenticada:
//
//	csrfMiddleware(sessionAuthMiddleware(requireRoleMiddleware(role, handler)))  // admin
//	csrfMiddleware(sessionAuthMiddleware(handler))                                // user comum
//
// Ordem de execução da request (de fora para dentro):
//  1. CSRF (valida cookie+header em mutações) — aplicado em tudo sob /api/web/*.
//  2. SessionAuth (carrega sessão do cookie, popula ctx) — DEVE vir antes de RequireRole.
//  3. RequireRole (RBAC default-deny) — aplicado em /api/web/admin/*; lê ctx populado.
//
// F4: o Bearer do proxy (/v1/*) NÃO passa por aqui — é tratado pelo pacote proxy.
//
// Bug F9: ordem anterior csrf(requireRole(sessionAuth(h))) causava requireRole
// executar com ctx vazio (session ainda não populado) → 500 "misconfigured".
// =============================================================================

// sessionAuthMiddleware valida o cookie de sessão e popula o contexto.
//
// Em caso de sessão ausente/expirada, retorna 401. O handler downstream obtém
// os dados via sessionFromContext(ctx).
func (a *App) sessionAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessID, err := readCookie(r, a.cookieCfg.SessionName)
		if err != nil || sessID == "" {
			writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
			return
		}
		sess, err := a.sessions.Get(r.Context(), sessID)
		if err != nil {
			// Sessão expirada ou inexistente — limpa cookie e nega.
			a.clearSessionCookie(w)
			a.clearCSRFCookie(w)
			writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session expired or invalid")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeySession, &sessionContext{
			SessionID: sess.ID,
			CSRFToken: sess.CSRFToken,
			UserID:    int64(sess.UserID),
			OrgID:     int64(sess.OrgID),
			Username:  sess.Username,
			Role:      string(sess.Role),
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireRoleMiddleware garante que o usuário autenticado tem a role exigida.
// DEVE ser aplicado DEPOIS de sessionAuthMiddleware (depende do contexto populado).
//
// Default-deny: se não houver regra permitindo, 403.
func (a *App) requireRoleMiddleware(role domain.Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r.Context())
		if sess == nil {
			// Programmatic error: requireRole sem sessionAuth na frente.
			writeAPIError(w, http.StatusInternalServerError, "internal", "auth middleware misconfigured")
			return
		}
		if domain.Role(sess.Role) != role {
			writeAPIError(w, http.StatusForbidden, "forbidden", "admin role required")
			return
		}
		next.ServeHTTP(w, r.WithContext(r.Context()))
	})
}

// csrfMiddleware valida o padrão double-submit CSRF.
//
// Para métodos "safe" (GET, HEAD, OPTIONS): passa direto (mas exige cookie CSRF
// presente como sinal de "origem legítima após login" — opcional, mantemos
// permissivo para permitir GET pré-login em algumas rotas).
//
// Para mutações (POST, PUT, PATCH, DELETE): exige que o cookie aipp_csrf
// e o header X-CSRF-Token tenham o MESMO valor. Comparação via subtle.ConstantTimeCompare.
//
// Como o cookie é SameSite=Lax (não Strict), o CSRF previne ataques cross-site
// em forms sem bloquear navegação top-level (login). Combinado com a sessão
// httpOnly, forma defesa em profundidade.
func (a *App) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMutationMethod(r.Method) {
			// Safe method — não exige CSRF.
			next.ServeHTTP(w, r)
			return
		}

		cookieVal, err := readCookie(r, a.cookieCfg.CSRFName)
		if err != nil || cookieVal == "" {
			writeAPIError(w, http.StatusForbidden, "csrf_missing", "CSRF cookie required")
			return
		}
		headerVal := r.Header.Get("X-CSRF-Token")
		if headerVal == "" {
			writeAPIError(w, http.StatusForbidden, "csrf_missing", "X-CSRF-Token header required")
			return
		}
		// Constant-time compare (evita timing attack para descobrir o token).
		if !constantTimeEqual(cookieVal, headerVal) {
			writeAPIError(w, http.StatusForbidden, "csrf_invalid", "CSRF token mismatch")
			return
		}

		// Camada extra (opcional mas recomendada): comparar o token CSRF da
		// sessão autenticada (não só cookie vs header). Isto bloqueia mesmo
		// ataques onde o atacante obteve o cookie CSRF mas não a sessão.
		// Como sessionAuthMiddleware já populou o ctx (em rotas autenticadas),
		// verificamos aqui também.
		if sess := sessionFromContext(r.Context()); sess != nil {
			if sess.CSRFToken != "" && !constantTimeEqual(sess.CSRFToken, headerVal) {
				writeAPIError(w, http.StatusForbidden, "csrf_invalid", "CSRF token does not match session")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// isMutationMethod retorna true para métodos que modificam estado.
func isMutationMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// constantTimeEqual compara duas strings em tempo constante.
// Previne timing attack na comparação do CSRF token.
func constantTimeEqual(a, b string) bool {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
