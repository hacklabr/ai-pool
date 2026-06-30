// Package admin implementa a UI e API administrativa do AI Provider Pool.
//
// Separação de camadas (spec §3.6.5):
//
//	/v1/*          → Proxy Core (Bearer sk-aipp-...)              [pacote proxy]
//	/web/*         → UI pública (login, logout) — sem auth prévia
//	/web/{dashboard,admin/*,me/*} → UI autenticada (Session)       [este pacote]
//	/api/web/*     → API admin autenticada (Session + CSRF)       [este pacote]
//	    /me/*      → próprio usuário (qualquer role)
//	    /admin/*   → gestão (role=admin)
//
// Auth: SESSÃO server-side cookie httpOnly + CSRF double-submit. SEGREGADA do
// Bearer do proxy. AuthFilter ramifica por rota.
//
// Mux PRÓPRIO via net/http.ServeMux (não usa web.Router do Beego). Templates
// são renderizados via html/template (Go stdlib) com HTMX para interatividade.
// Assets estáticos (CSS próprio) servidos via embed.FS; JS libs (HTMX, Alpine,
// ECharts) via CDN por pragmatismo (documentado em web/README.md).
package admin

import (
	"io/fs"
	"net/http"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// App carrega todas as dependências da admin/UI via struct (DI explícita).
//
// Mantida como struct (não globais) para facilitar testes com mocks e permitir
// múltiplas instâncias (ex.: testes paralelos).
type App struct {
	pool         *pgxpool.Pool
	sessions     domain.SessionStore
	userRepo     *postgres.UserRepo
	tokenRepo    *postgres.TokenRepo
	providerRepo *postgres.ProviderRepo
	keyRepo      *postgres.KeyRepo
	// usageRepo e auditRepo expõem leituras para o Dashboard e a tela de
	// Auditoria. São nil-safe: se não inicializados (ex.: testes sem DB),
	// os handlers retornam 503 graceful em vez de nil-panic.
	usageRepo *postgres.UsageLogRepo
	auditRepo *postgres.AuditRepo
	// renderer renderiza templates HTML. Inicializado em SetTemplates().
	// Se nil (ex.: testes), handlers HTML respondem 503.
	renderer *templateRenderer
	// staticFS serve assets estáticos (CSS). Inicializado em SetStaticFS().
	staticFS  fs.FS
	pepper    []byte
	fakeHash  string // hash fake para timing equalization no login
	cookieCfg cookieConfig
	devMode   bool // influencia Secure flag do cookie (dev permite http)
	// rateLimiter (F6) — usado para rate-limit do /web/login por IP (anti brute-force).
	rateLimiter domain.RateLimiter
	// encryptor (F7) — cifra valores de chaves de provedores ao criar.
	// Em DEV_MODE pode ser nil (usa plaintext direto); em prod é AESGCMCrypto.
	encryptor domain.KeyEncryptor
}

// cookieConfig parametriza flags dos cookies de sessão/CSRF.
type cookieConfig struct {
	SessionName string // "aipp_sess"
	CSRFName    string // "aipp_csrf"
	Domain      string // vazio = host corrente
	Path        string // "/"
	// Secure=false em DEV_MODE (permite http://localhost); true em prod.
	Secure bool
	// SameSite=Lax permite que o cookie acompanhe navegação top-level (login),
	// mas bloqueia CSRF cross-site em forms (combinado com double-submit CSRF).
	SameSite http.SameSite
	// MaxAge da sessão em segundos (alinhado ao TTL do SessionStore).
	MaxAge int
}

// NewApp cria o App admin com todas as dependências.
//
// fakeHash deve vir de domain.NewFakePasswordHash() — usado em timing
// equalization no login (usuário inexistente → compara contra fake hash).
func NewApp(
	pool *pgxpool.Pool,
	sessions domain.SessionStore,
	pepper []byte,
	devMode bool,
) *App {
	a := &App{
		pool:         pool,
		sessions:     sessions,
		userRepo:     postgres.NewUserRepo(pool),
		tokenRepo:    postgres.NewTokenRepo(pool),
		providerRepo: postgres.NewProviderRepo(pool),
		keyRepo:      postgres.NewKeyRepo(pool),
		// usageRepo e auditRepo inicializados aqui (leitura para Dashboard/Audit).
		usageRepo: postgres.NewUsageLogRepo(pool),
		auditRepo: postgres.NewAuditRepo(pool),
		pepper:    pepper,
		fakeHash:  domain.NewFakePasswordHash(),
		cookieCfg: cookieConfig{
			SessionName: "aipp_sess",
			CSRFName:    "aipp_csrf",
			Path:        "/",
			Secure:      !devMode, // dev permite http; prod exige https
			SameSite:    http.SameSiteLaxMode,
			MaxAge:      int(domain.SessionTTLDefault.Seconds()),
		},
		devMode: devMode,
	}
	return a
}

// SetRateLimiter injeta o rate limiter (F6) para proteção brute-force do login.
func (a *App) SetRateLimiter(rl domain.RateLimiter) {
	a.rateLimiter = rl
}

// SetEncryptor injeta o cifrador (F7) para envelope encryption de chaves.
func (a *App) SetEncryptor(enc domain.KeyEncryptor) {
	a.encryptor = enc
}

// SetTemplates injeta o renderizador de templates HTML (F8). Sem isto, os
// handlers HTML respondem 503 (permite que o App funcione sem templates em
// ambientes de teste ou se o embed.FS falhar ao carregar).
func (a *App) SetTemplates(r *templateRenderer) {
	a.renderer = r
}

// SetStaticFS injeta o fs.FS que serve assets estáticos (CSS). Tipicamente
// aponta para o subtree "static" do embed.FS do pacote web.
func (a *App) SetStaticFS(s fs.FS) {
	a.staticFS = s
}

// Handler retorna o http.Handler para as rotas admin (prefixo /web e /api/web).
//
// O main.go monta este handler no mux composto para os paths correspondentes
// (tudo que NÃO é /healthz, /readyz, /v1/* cai aqui se casar com /web ou
// /api/web; o placeholder HTML "/" também é tratado aqui).
//
// Estrutura de rotas (F8):
//
//	Públicas (sem auth):
//	  GET  /web/login            → página de login (HTML)
//	  POST /web/login            → autentica, cria sessão
//	  POST /web/logout           → encerra sessão
//	  GET  /                     → redirect para dashboard/login conforme sessão
//
//	Estáticos (sem auth):
//	  GET  /static/*             → CSS próprio (embed.FS)
//
//	UI autenticada (Session, role-agnostic):
//	  GET  /web/dashboard        → dashboard admin (role=admin) ou "meu uso"
//	  GET  /web/me/tokens        → meus tokens (HTML)
//	  GET  /web/me/usage         → meu uso (HTML)
//	  GET  /web/me               → perfil próprio (alias /api/web/me)
//
//	UI admin (Session + role=admin):
//	  GET  /web/admin/users      → gestão de usuários (HTML)
//	  GET  /web/admin/providers  → gestão de provedores (HTML)
//	  GET  /web/admin/keys       → gestão de chaves (HTML)
//	  GET  /web/admin/audit      → auditoria (HTML)
//
//	API JSON autenticada (Session + CSRF em mutações):
//	  /api/web/me/*              → próprio usuário (tokens, dados)
//	  /api/web/admin/users       → CRUD usuários
//	  /api/web/admin/providers   → CRUD provedores
//	  /api/web/admin/keys        → CRUD chaves + force-enable/disable
//	  /api/web/admin/dashboard   → JSON para gráficos ECharts (NOVO F8)
//	  /api/web/admin/audit       → JSON paginado para auditoria (NOVO F8)
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	// === Estáticos: CSS próprio (embed.FS) ===
	// JS libs (HTMX/Alpine/ECharts) via CDN — ver web/README.md.
	if a.staticFS != nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(a.staticFS))))
	}

	// === Rotas públicas (sem auth prévia) ===
	mux.HandleFunc("POST /web/login", a.handleLogin)
	mux.HandleFunc("POST /web/logout", a.handleLogout)
	mux.HandleFunc("GET /web/login", a.handleLoginPage)
	// Usamos /{$} (match EXATO em "/", sem subtree) para evitar conflito com
	// os mounts "/web/me/", "/web/dashboard", "/web/admin/", "/api/web/me/",
	// "/api/web/admin/" — o Go 1.22+ ServeMux panic se "GET /" (subtree)
	// coexiste com patterns sem método mais específicos em path. {$} força
	// match somente em "GET /" literal; outras rotas caem em seus handlers
	// específicos, e paths não-casados retornam 404 (comportamento desejado).
	mux.HandleFunc("GET /{$}", a.handleIndex) // redirect baseado em sessão

	// === Páginas HTML autenticadas (/web/me/*) — qualquer role ===
	// Envoltas em sessionAuthMiddleware (sem exigir CSRF em GET — padrão do
	// csrfMiddleware). Mutations via HTMX exigirão CSRF no header X-CSRF-Token.
	mePages := http.NewServeMux()
	mePages.HandleFunc("GET /web/me/tokens", a.handlePageMyTokens)
	mePages.HandleFunc("GET /web/me/usage", a.handlePageMyUsage)
	mePages.HandleFunc("GET /web/me", a.handlePageMyTokens) // alias
	mux.Handle("/web/me/", a.sessionAuthMiddleware(mePages))

	// === Páginas HTML autenticadas (role-agnostic) — dashboard ===
	// Dashboard é mostrado para qualquer role autenticada; admin vê KPIs globais,
	// user comum vê apenas "meu uso" (o handler decide conforme role).
	dashPages := http.NewServeMux()
	dashPages.HandleFunc("GET /web/dashboard", a.handlePageDashboard)
	mux.Handle("/web/dashboard", a.sessionAuthMiddleware(dashPages))

	// === Páginas HTML admin (/web/admin/*) — role=admin ===
	adminPages := http.NewServeMux()
	adminPages.HandleFunc("GET /web/admin/users", a.handlePageAdminUsers)
	adminPages.HandleFunc("GET /web/admin/users/{id}", a.handlePageAdminUserDetail)
	adminPages.HandleFunc("GET /web/admin/providers", a.handlePageAdminProviders)
	adminPages.HandleFunc("GET /web/admin/models", a.handlePageAdminModels)
	adminPages.HandleFunc("GET /web/admin/keys", a.handlePageAdminKeys)
	adminPages.HandleFunc("GET /web/admin/audit", a.handlePageAdminAudit)
	mux.Handle("/web/admin/", a.sessionAuthMiddleware(a.requireRoleMiddleware(domain.RoleAdmin, adminPages)))

	// === /api/web/me/* — próprio usuário (qualquer role) ===
	// AuthFilter (Session+CSRF) aplicado via grupo.
	me := http.NewServeMux()
	me.HandleFunc("GET /api/web/me", a.handleGetMe)
	me.HandleFunc("GET /api/web/me/usage", a.handleMyUsageData)
	me.HandleFunc("GET /api/web/me/tokens", a.handleListMyTokens)
	me.HandleFunc("POST /api/web/me/tokens", a.handleCreateMyToken)
	me.HandleFunc("DELETE /api/web/me/tokens/{id}", a.handleRevokeMyToken)
	mux.Handle("/api/web/me/", a.withSessionAuth(me))

	// === /api/web/admin/* — gestão (role=admin) ===
	adminGrp := http.NewServeMux()
	adminGrp.HandleFunc("GET /api/web/admin/users", a.handleAdminListUsers)
	adminGrp.HandleFunc("POST /api/web/admin/users", a.handleAdminCreateUser)
	adminGrp.HandleFunc("PATCH /api/web/admin/users/{id}/status", a.handleAdminSetUserStatus)
	adminGrp.HandleFunc("POST /api/web/admin/users/{id}/password", a.handleAdminSetUserPassword)
	adminGrp.HandleFunc("GET /api/web/admin/providers", a.handleAdminListProviders)
	adminGrp.HandleFunc("POST /api/web/admin/providers", a.handleAdminCreateProvider)
	adminGrp.HandleFunc("GET /api/web/admin/models", a.handleAdminListModels)
	adminGrp.HandleFunc("POST /api/web/admin/models", a.handleAdminCreateModel)
	adminGrp.HandleFunc("GET /api/web/admin/keys", a.handleAdminListKeys)
	adminGrp.HandleFunc("POST /api/web/admin/keys", a.handleAdminCreateKey)
	adminGrp.HandleFunc("POST /api/web/admin/keys/{id}/force-disable", a.handleAdminForceDisableKey)
	adminGrp.HandleFunc("POST /api/web/admin/keys/{id}/force-enable", a.handleAdminForceEnableKey)
	// NOVO F8: endpoints JSON para Dashboard (ECharts) e Auditoria.
	adminGrp.HandleFunc("GET /api/web/admin/dashboard", a.handleAdminDashboardData)
	adminGrp.HandleFunc("GET /api/web/admin/audit", a.handleAdminAuditData)
	mux.Handle("/api/web/admin/", a.withAdminAuth(adminGrp))

	return mux
}

// withSessionAuth envolve o handler com SessionAuth + CSRF (para mutações).
// Usado para /api/web/me/* (qualquer role).
//
// Ordem de execução (request): csrf → sessionAuth → handler.
//   - csrf valida cookie+header em mutações (double-submit).
//   - sessionAuth popula ctx com a sessão do cookie.
func (a *App) withSessionAuth(h http.Handler) http.Handler {
	return a.csrfMiddleware(a.sessionAuthMiddleware(h))
}

// withAdminAuth envolve com SessionAuth + CSRF + RequireRole(admin).
// Usado para /api/web/admin/*.
//
// Ordem de execução (request): csrf → sessionAuth → requireRole → handler.
//   - csrf valida CSRF (double-submit, não depende de sessão).
//   - sessionAuth popula ctx com a sessão (deve vir ANTES de requireRole).
//   - requireRole checa role do ctx populado (default-deny se não for admin).
//
// Bug fix F9: ordem anterior csrf(requireRole(sessionAuth(h))) fazia
// requireRole executar ANTES de sessionAuth popular o ctx, retornando
// sempre 500 "auth middleware misconfigured" para /api/web/admin/*. A ordem
// abaixo inverte sessionAuth/requireRole para que o ctx seja populado a tempo.
func (a *App) withAdminAuth(h http.Handler) http.Handler {
	return a.csrfMiddleware(a.sessionAuthMiddleware(a.requireRoleMiddleware(domain.RoleAdmin, h)))
}
