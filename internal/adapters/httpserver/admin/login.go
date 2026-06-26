package admin

import (
	"context"
	"net/http"
	"time"

	"github.com/rafael/ai-pool/internal/domain"
)

// =============================================================================
// Handlers de autenticação web (login/logout).
//
// Rotas:
//   - GET  /web/login    → página de login (HTML mínimo em F4; HTMX em F8).
//   - POST /web/login    → valida credenciais, cria sessão, seta cookies.
//   - POST /web/logout   → deleta sessão, limpa cookies.
//
// Spec §3.3.5: Argon2id + sessão server-side cookie httpOnly + CSRF.
// Anti-enumeration: resposta 401 idêntica para "usuário inexistente" e
// "senha errada" (timing equalization via fakeHash).
// =============================================================================

// loginRequest payload do POST /web/login.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginResponse payload de sucesso.
type loginResponse struct {
	User      userPublic `json:"user"`
	CSRFToken string     `json:"csrf_token"` // frontend precisa para mutações
}

// userPublic é a representação pública segura (sem password_hash).
type userPublic struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	OrgID    int64  `json:"org_id"`
}

// handleLoginPage renderiza a página de login (HTML).
//
// F8: usa template server-rendered (layouts/base.html minimal para login +
// pages/login.html). Estrutura multi-step-ready: Alpine x-data controla
// step 1 (senha, ativo MVP) e step 2 (MFA, desenhado mas oculto). Slot OAuth
// reservado no rodapé (vazio no MVP).
//
// Se a sessão já é válida (cookie presente e válido), redireciona para o
// dashboard — evita loop desnecessário pelo login.
func (a *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// Auto-redirect se já logado (conveniência).
	if sessID, err := readCookie(r, a.cookieCfg.SessionName); err == nil && sessID != "" {
		if sess, err := a.sessions.Get(r.Context(), sessID); err == nil && sess != nil {
			http.Redirect(w, r, "/web/dashboard", http.StatusSeeOther)
			return
		}
	}
	if a.renderer == nil {
		// Fallback F4 (HTML inline) se renderer não inicializado.
		a.renderLoginFallback(w)
		return
	}
	data := pageData{
		Title:     "Entrar — AI Provider Pool",
		ActiveNav: "login",
		DevMode:   a.devMode,
	}
	if err := a.renderer.renderPage(w, "pages/login.html", data); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not render login page")
		return
	}
}

// renderLoginFallback escreve HTML inline mínimo quando o renderer não está
// disponível (ex.: testes sem embed.FS). Mantém o app funcional para smoke tests.
func (a *App) renderLoginFallback(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="pt-BR"><head><meta charset="utf-8"><title>AI Provider Pool — Login</title>
<style>body{font-family:system-ui;max-width:360px;margin:4rem auto;padding:0 1rem}
input{display:block;width:100%;padding:.5rem;margin:.3rem 0;box-sizing:border-box}
button{padding:.6rem 1.2rem;margin-top:.5rem}</style></head>
<body><h1>AI Provider Pool</h1>
<p>Renderer de templates indisponível. Use a API JSON diretamente:</p>
<pre>POST /web/login
{"username":"...","password":"..."}</pre>
</body></html>`))
}

// handleLogin valida credenciais e cria sessão.
//
// Fluxo:
//  0. Rate-limit por IP (10 req/min) — proteção brute-force (spec §3.3.5).
//  1. Parse JSON body (strict, max 4KB).
//  2. Lookup usuário por username (na org default do MVP).
//  3. Se encontrado: VerifyPassword(Argon2id). Se NÃO encontrado: compara
//     senha contra fakeHash (timing equalization) e descarta o resultado.
//  4. Se credenciais OK: cria sessão (32 bytes), persiste no store, seta
//     cookies (sessão httpOnly + CSRF legível por JS).
//  5. Retorna 200 + dados do usuário + CSRF token.
//
// Em TODOS os casos de falha, resposta é 401 genérica ("invalid credentials").
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	// 0. Rate-limit por IP (10/min) — anti brute-force (spec §3.3.5).
	if a.rateLimiter != nil {
		ip := clientIP(r)
		allowed, _, err := a.rateLimiter.Allow(r.Context(), "login_ip:"+ip)
		if err == nil && !allowed {
			writeAPIError(w, http.StatusTooManyRequests, "rate_limited",
				"too many login attempts; please try again later")
			return
		}
	}

	var req loginRequest
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeAPIError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
		return
	}

	ctx := r.Context()
	user, err := a.userRepo.GetByUsername(ctx, domain.DefaultOrganizationID, req.Username)

	// Timing equalization: mesmo se usuário não existe, comparamos senha contra
	// um hash fake para ter tempo similar ao caso "senha errada".
	passwordOK := false
	if err == nil && user != nil {
		// Usuário existe — verifica senha real.
		if user.Status != domain.UserStatusActive {
			// Usuário desabilitado: tratamos como credenciais inválidas (não
			// revelamos que existe mas está desabilitado). Ainda assim gastamos
			// o tempo do Argon2id para timing equalization com usuário ativo.
			_ = domain.VerifyPassword([]byte(req.Password), string(user.PasswordHash))
		} else {
			passwordOK = domain.VerifyPassword([]byte(req.Password), string(user.PasswordHash)) == nil
		}
	} else {
		// Usuário inexistente — compara contra fake hash (descarta resultado).
		_ = domain.VerifyPassword([]byte(req.Password), a.fakeHash)
		passwordOK = false
	}

	if !passwordOK {
		writeAPIError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
		return
	}

	// Credenciais OK — cria sessão.
	sessID, csrfTok, err := a.createSession(ctx, user)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not create session")
		return
	}

	a.setSessionCookie(w, sessID)
	a.setCSRFCookie(w, csrfTok)

	writeJSON(w, http.StatusOK, loginResponse{
		User: userPublic{
			ID:       int64(user.ID),
			Username: user.Username,
			Role:     string(user.Role),
			OrgID:    int64(user.OrgID),
		},
		CSRFToken: csrfTok,
	})
}

// handleLogout deleta a sessão e limpa cookies. Idempotente.
//
// CSRF exigido (mutação). Mesmo que o cookie esteja ausente (sessão já expirou),
// retornamos 200 (UX: não punir duplo clique em logout).
func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sessID, err := readCookie(r, a.cookieCfg.SessionName); err == nil && sessID != "" {
		_ = a.sessions.Delete(r.Context(), sessID)
	}
	a.clearSessionCookie(w)
	a.clearCSRFCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// handleIndex é o ponto de entrada raiz. Redireciona para dashboard se
// autenticado, ou para login caso contrário. Esta lógica vive aqui (não no
// middleware) para que "/" seja sempre acessível mesmo sem sessão.
func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if sessID, err := readCookie(r, a.cookieCfg.SessionName); err == nil && sessID != "" {
		if sess, err := a.sessions.Get(r.Context(), sessID); err == nil && sess != nil {
			// Admin → dashboard global; user comum → meu uso.
			if sess.Role == domain.RoleAdmin {
				http.Redirect(w, r, "/web/dashboard", http.StatusSeeOther)
			} else {
				http.Redirect(w, r, "/web/me/usage", http.StatusSeeOther)
			}
			return
		}
	}
	http.Redirect(w, r, "/web/login", http.StatusSeeOther)
}

// createSession gera IDs aleatórios, monta a Session, persiste no store.
// Retorna (sessionID, csrfToken, error).
//
// Ambos IDs têm 32 bytes de entropia (256 bits) — inviável brute-force/adivinhar.
func (a *App) createSession(ctx context.Context, user *domain.User) (string, string, error) {
	sessID, err := randomTokenString(32)
	if err != nil {
		return "", "", err
	}
	csrfTok, err := randomTokenString(32)
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	sess := &domain.Session{
		ID:         sessID,
		UserID:     user.ID,
		OrgID:      user.OrgID,
		Username:   user.Username,
		Role:       user.Role,
		CreatedAt:  now,
		ExpiresAt:  now.Add(domain.SessionTTLDefault),
		LastSeenAt: now,
		CSRFToken:  csrfTok,
	}
	if err := a.sessions.Create(ctx, sess); err != nil {
		return "", "", err
	}
	return sessID, csrfTok, nil
}
