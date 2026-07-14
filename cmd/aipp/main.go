// Package main é o entrypoint do AI Provider Pool.
//
// Composição (F4+):
//
//  1. config.Load() + logger estruturado.
//  2. Detecção de subcomando CLI (ex.: --bootstrap-admin) ANTES de iniciar
//     servidor — útil para setup administrativo (criar admin inicial).
//  3. pgxpool.New (com ping inicial — falha bloqueia startup).
//  4. Adapters:
//     - postgres.TokenValidator (DB hit, HMAC+pepper).
//     - postgres.KeyResolver (primeira usable + decryptor).
//     - store.IdentityDecryptor (dev passthrough; AES-GCM em F7).
//     - store.NewSessionStore (admin web sessions).
//     - admin.NewApp (handler /web e /api/web).
//     - proxy.NewUpstreamClient + proxy.NewServer (handler /v1/* real).
//  5. http.Server único, composeHandler com 3 sub-handlers:
//     Proxy Core > Admin App > Beego placeholder.
//  6. Graceful shutdown (SIGTERM/SIGINT) — drena conexões, cancela streams.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/beego/beego/v2/server/web"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hacklabr/ai-pool/internal/adapters/crypto"
	"github.com/hacklabr/ai-pool/internal/adapters/httpserver/admin"
	"github.com/hacklabr/ai-pool/internal/adapters/httpserver/failover"
	"github.com/hacklabr/ai-pool/internal/adapters/httpserver/proxy"
	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/adapters/security"
	"github.com/hacklabr/ai-pool/internal/adapters/store"
	"github.com/hacklabr/ai-pool/internal/config"
	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/routing"
	"github.com/hacklabr/ai-pool/internal/usage"
	"github.com/hacklabr/ai-pool/migrations"
	webassets "github.com/hacklabr/ai-pool/web"
)

func main() {
	// Flags CLI para subcomandos administrativos.
	var (
		bootstrapAdminUser = flag.String("bootstrap-admin-username", "", "create admin user (idempotent) with this username; reads password from AIPP_BOOTSTRAP_ADMIN_PASSWORD env or prompts")
		bootstrapAdminPass = flag.String("bootstrap-admin-password", "", "password for bootstrap admin (prefer env AIPP_BOOTSTRAP_ADMIN_PASSWORD over this flag)")
		// migrateOnly: roda RunMigrations contra DATABASE_URL (embed.FS) e sai.
		// Usado pelo Helm pre-install/pre-upgrade Job hook (spec §3.5.1, F10).
		// Em produção NÃO rodamos migrations no startup do app — apenas via Job
		// isolado para evitar N réplicas competindo e para falhar o deploy
		// antes de qualquer pod subir com schema inconsistente.
		migrateOnly = flag.Bool("migrate-only", false, "run migrations from embedded FS against DATABASE_URL and exit (exit 0 on success, 1 on failure); used by Helm Job hook")
	)
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("config load failed", "err", err)
		os.Exit(1)
	}
	logger := buildLogger(cfg)

	// === Modo CLI: migrate-only (Helm Job hook) ===
	// Roda antes de qualquer outra coisa: carrega config, valida DATABASE_URL,
	// aplica migrations do embed.FS, loga a versão final e sai.
	// Não abre pool pgx, não inicia servidor, não toca em KEK/pepper.
	if *migrateOnly {
		os.Exit(runMigrateOnly(cfg, logger))
	}

	// === Modo CLI: bootstrap-admin ===
	// Cria usuário role=admin se não existe. Idempotente.
	if *bootstrapAdminUser != "" {
		os.Exit(runBootstrapAdmin(cfg, logger, *bootstrapAdminUser, *bootstrapAdminPass))
	}

	pepper, err := cfg.LoadTokenPepper()
	if err != nil {
		logger.Error("load token pepper", "err", err)
		os.Exit(1)
	}

	// === Migrations automáticas em DEV_MODE ===
	// Em produção (DevMode=false), as migrations rodam via Helm Job hook
	// (spec §3.5.1) — não no startup do app. Em DEV, rodamos automaticamente
	// para que `docker compose up` traga o schema pronto sem passo manual.
	if cfg.DevMode {
		runMigrationsOnStartup(cfg, logger)
	}

	// === Pool PostgreSQL ===
	// Em DEV_MODE sem DATABASE_URL, o pool é opcional — o proxy/admin sobem
	// com deny-by-default para auth (seguro: nada passa sem DB).
	pool := newPoolOrSkip(cfg, logger)

	// === Bootstrap admin automático em DEV_MODE ===
	// Se AIPP_BOOTSTRAP_ADMIN_USERNAME/PASSWORD estão setados, cria admin
	// inicial (idempotente). Facilita `docker compose up` trazer admin pronto.
	// Em produção, usar `aipp --bootstrap-admin-username=...` via Helm Job
	// ou kubectl exec. Em DEV sem essas envs, pula silenciosamente.
	if cfg.DevMode && pool != nil {
		bootstrapAdminOnStartup(cfg, logger, pool)
	}

	// === Adapters do domínio (interfaces) ===
	tokenValidator := newTokenValidator(pool, pepper, logger)
	upstreamClient := proxy.NewUpstreamClient(cfg)

	// === Routing Engine (F5) ===
	// Substitui o KeyResolver mínimo do F3 por LRU + Afinidade + Auto-disable +
	// Scheduler de reativação. Se pool==nil (dev sem DB), usa stub deny-by-default.
	var (
		keyResolver   domain.KeyResolver = denyAllKeyResolver{}
		routingEngine domain.RoutingEngine
		scheduler     *routing.ReactivationScheduler
		schedCancel   context.CancelFunc
	)
	if pool != nil {
		engine, engScheduler, engCancel := buildRoutingEngine(cfg, logger, pool)
		keyResolver = engine
		routingEngine = engine
		scheduler = engScheduler
		schedCancel = engCancel
	}

	// === Adapters admin/UI ===
	// Em DEV sem DB, admin App não tem funcionalidade real — mas mantemos o
	// handler montado para que endpoints respondam 401/503 ao invés de 404.
	var (
		sessionStore domain.SessionStore
		adminApp     *admin.App
	)
	if pool != nil {
		sessionStore = store.NewSessionStore()
		adminApp = admin.NewApp(pool, sessionStore, pepper, cfg.DevMode, cfg.PublicURL)
		go startSessionCleanup(sessionStore, 10*time.Minute, logger)
	} else {
		// Stub admin que responde 503 para tudo (precisa DB).
		sessionStore = nil
		adminApp = nil
	}

	logger.Info("adapters initialized",
		"mode", modeStr(cfg.DevMode),
		"ssrf_allow_http", cfg.SSRFAllowInsecureHTTP,
	)

	// === Rate Limiter (F6) ===
	// Token bucket in-memory. Defaults: burst 10, 60 req/min.
	// Para o /web/login: limitamos por IP com 10/min (configurado no admin App).
	rateLimiter := store.NewRateLimiter(
		store.WithRateLimitDefaults(cfg.RateLimitBurst, cfg.RateLimitPerMin),
	)
	// Configura rate-limit do login (por IP): 10 req/min.
	// SetLimit cria o bucket sob demanda no primeiro request de cada IP.
	// Como o bucket é por escopo, setamos apenas defaults; IPs individuais
	// usam o default do RateLimiter. Para personalizar por IP, chamar SetLimit
	// dinamicamente. Por ora, configuramos um escopo "login_ip:*" que herda
	// defaults — refinamos em F8.
	loginBurst := 10
	loginRatePerMin := 10
	_ = rateLimiter.SetLimit(context.Background(), "login_ip:_default", loginBurst, loginRatePerMin)

	// === Usage Collector (F6) ===
	// Canal bufferizado + goroutine worker. Batch insert assíncrono.
	var usageCollector domain.UsageCollector = domain.NoopUsageCollector{}
	var rollupJob *usage.RollupJob
	if pool != nil {
		usageRepo := postgres.NewUsageLogRepo(pool)
		realCollector := usage.NewCollector(usage.DefaultCollectorConfig(), usageRepo)
		realCollector.Start(context.Background())
		usageCollector = realCollector
		// Rollup job: agrega usage_logs → usage_rollups a cada 5min.
		rollupJob = usage.NewRollupJob(usageRepo, cfg.DefaultOrgID, time.Minute, logger)
	}

	// === HTTP servers ===
	// Beego placeholder "/" — adminApp.Handler() já trata "/" também; mas
	// mantemos registro Beego para compat futura com templates em F8.
	web.Router("/", &placeholderBeegoController{})
	proxyServer := proxy.NewServer(cfg, logger, tokenValidator, keyResolver, upstreamClient)

	// F6: injeta rate limiter + usage collector no proxy.
	proxyServer.SetRateLimiter(rateLimiter)
	proxyServer.SetUsageCollector(usageCollector)
	// F6: injeta rate limiter no admin (para /web/login brute-force protection).
	if adminApp != nil {
		adminApp.SetRateLimiter(rateLimiter)
		// F7: injeta encryptor no admin (cifra chaves ao criar).
		adminApp.SetEncryptor(buildEncryptor(cfg, logger))
		// F8: injeta templates HTML (embed.FS do pacote web) e estáticos.
		// Em caso de erro de parse, loga mas NÃO derruba o startup — handlers
		// HTML respondem 503 graceful (App continua funcional para a API JSON).
		if renderer, err := admin.NewTemplateRenderer(webassets.TemplatesFS()); err != nil {
			logger.Error("F8: failed to load UI templates; HTML pages will return 503", "err", err)
		} else {
			adminApp.SetTemplates(renderer)
			adminApp.SetStaticFS(webassets.StaticFS())
			logger.Info("F8: UI templates loaded", "static_served", true)
		}
	}

	// F5: injeta FailoverChain no proxy se routingEngine está disponível.
	if routingEngine != nil {
		wrapper := failover.NewChainWrapper(
			routingEngine,
			proxyServer,
			upstreamClient,
			routing.DefaultBackoffConfig(),
			logger,
		)
		proxyServer.SetFailoverChain(wrapper)
		logger.Info("failover chain enabled (LRU + affinity + auto-disable)")
	}

	// Inicia scheduler de reativação (background goroutine).
	if scheduler != nil {
		schedCtx, schedCancel2 := context.WithCancel(context.Background())
		go scheduler.Start(schedCtx)
		defer schedCancel2()
		defer schedCancel() // stop flusher goroutine from buildRoutingEngine
	}

	// Inicia rollup job (background goroutine).
	if rollupJob != nil {
		rollupCtx, rollupCancel := context.WithCancel(context.Background())
		go rollupJob.Start(rollupCtx)
		defer rollupCancel()
	}

	// Admin handler: real se DB conectado, stub 503 caso contrário.
	var adminHandler http.Handler
	if adminApp != nil {
		adminHandler = adminApp.Handler()
	} else {
		adminHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeServiceUnavailable(w, "admin endpoints require DATABASE_URL")
		})
	}

	rootHandler := composeHandler(proxyServer.Handler(), adminHandler, web.BeeApp.Handlers)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           rootHandler,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	// === Graceful shutdown ===
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-stop:
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		logger.Error("http server error", "err", err)
		os.Exit(1)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown error", "err", err)
		}
		// Fecha o pool PG se foi aberto. Em DEV_MODE sem DATABASE_URL, pool é
		// nil (não há nada para fechar) — chamar Close em nil panica.
		if pool != nil {
			pool.Close()
		}
	}()
	wg.Wait()

	logger.Info("shutdown complete")
}

// runBootstrapAdmin cria um admin inicial se não existe. Idempotente.
//
// Uso:
//
//	AIPP_BOOTSTRAP_ADMIN_PASSWORD="..." ./aipp --bootstrap-admin-username=admin
//
// Ou via flag (menos seguro — aparece em ps):
//
//	./aipp --bootstrap-admin-username=admin --bootstrap-admin-password="..."
//
// Comportamento:
//   - Conecta ao DB (exige DATABASE_URL).
//   - Se o usuário já existe (qualquer role): NÃO altera. Apenas loga e sai 0.
//   - Se não existe: cria com role=admin, Argon2id hash, status=active.
//
// Retorna código de saída (0=OK, 1=erro, 2=config inválida).
func runBootstrapAdmin(cfg *config.Config, logger *slog.Logger, username, passwordFlag string) int {
	if cfg.DBDSN == "" {
		logger.Error("bootstrap-admin requires DATABASE_URL")
		return 2
	}
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(username) > 50 {
		logger.Error("invalid username (must be 3-50 chars)")
		return 2
	}

	// Password: env优先, depois flag, depois prompt stdin.
	password := os.Getenv("AIPP_BOOTSTRAP_ADMIN_PASSWORD")
	if password == "" {
		password = passwordFlag
	}
	if password == "" {
		logger.Error("password required (set AIPP_BOOTSTRAP_ADMIN_PASSWORD env or use --bootstrap-admin-password flag)")
		return 2
	}
	if len(password) < 12 {
		logger.Error("password must be at least 12 characters")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := postgres.NewPool(ctx, cfg)
	if err != nil {
		logger.Error("postgres connect failed", "err", err)
		return 1
	}
	defer pool.Close()

	userRepo := postgres.NewUserRepo(pool)

	// Idempotência: se já existe (qualquer role), loga e sai 0 sem modificar.
	if existing, err := userRepo.GetByUsername(ctx, domain.DefaultOrganizationID, username); err == nil && existing != nil {
		logger.Info("bootstrap-admin: user already exists, no changes",
			"username", username, "current_role", string(existing.Role))
		return 0
	}

	hash, err := domain.HashPassword([]byte(password))
	if err != nil {
		logger.Error("argon2id hash failed", "err", err)
		return 1
	}

	user := &domain.User{
		OrgID:        domain.DefaultOrganizationID,
		Username:     username,
		PasswordHash: []byte(hash),
		Role:         domain.RoleAdmin,
		Status:       domain.UserStatusActive,
	}
	if _, err := userRepo.Create(ctx, user); err != nil {
		logger.Error("create admin user failed", "err", err)
		return 1
	}

	// Log sem expor senha. Username é OK (não é segredo).
	logger.Info("bootstrap-admin: admin user created successfully",
		"username", username, "role", string(domain.RoleAdmin))
	fmt.Fprintf(os.Stderr, "OK: admin user %q created.\n", username)
	return 0
}

// runMigrateOnly é o handler do subcomando `aipp --migrate-only`.
//
// Carrega config (apenas para obter DATABASE_URL; ignora DevMode e outras
// validações de produção — o Job sempre roda em modo "produção" no sentido
// de que DBDSN é obrigatório), aplica migrations do embed.FS, loga a versão
// final do schema e retorna exit code:
//
//	0 — migrations aplicadas com sucesso (ou já estavam em dia, ErrNoChange).
//	1 — erro fatal (DSN vazio, falha ao abrir DB, falha ao aplicar).
//	2 — config inválida (raro; tratada pelo config.Load antes, mas defensivo).
//
// É usado pelo Helm pre-install/pre-upgrade Job hook (F10, spec §3.5.1).
// Roda isoladamente: não inicia servidor, não abre pool pgx (apenas database/sql
// dedicado, conforme postgres.RunMigrations), não toca em KEK/pepper.
//
// Idempotente: pode ser re-executado sem efeito colateral se já está em dia.
// Concurrence-safe: o golang-migrate usa advisory lock internamente, então
// múltiplos Jobs em paralelo (raro, mas possível em retry) não corrompem.
func runMigrateOnly(cfg *config.Config, logger *slog.Logger) int {
	if cfg.DBDSN == "" {
		logger.Error("--migrate-only: DATABASE_URL (or DB_DSN) is required")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	logger.Info("--migrate-only: applying migrations",
		"current_dev_mode", cfg.DevMode, // informativo; migrate-only não respeita DevMode
	)

	if err := postgres.RunMigrations(ctx, cfg.DBDSN, migrations.FS); err != nil {
		logger.Error("--migrate-only: migrations failed", "err", err)
		return 1
	}

	version, dirty, err := postgres.MigrationsVersion(ctx, cfg.DBDSN)
	if err != nil {
		logger.Warn("--migrate-only: could not read schema version after migration", "err", err)
	} else {
		logger.Info("--migrate-only: migrations applied successfully",
			"schema_version", version, "dirty", dirty,
		)
	}

	fmt.Fprintf(os.Stderr, "OK: schema at version %d (dirty=%v).\n", version, dirty)
	return 0
}

// runMigrationsOnStartup aplica todas as migrations pendentes a partir do
// embed.FS em migrations.FS. Em DEV_MODE: o startup falha rápido se migrations
// falham (es fail-fast). Em produção este caminho NÃO é executado — Helm Job
// hook roda migrations isoladamente (spec §3.5.1).
//
// Comportamento:
//   - DATABASE_URL vazio: loga warning e pula (dev sem DB).
//   - Sucesso: loga a versão final do schema.
//   - Falha: loga erro e os.Exit(1) — app não sobe com schema inconsistente.
func runMigrationsOnStartup(cfg *config.Config, logger *slog.Logger) {
	if cfg.DBDSN == "" {
		logger.Warn("DEV_MODE: skipping migrations on startup (DATABASE_URL empty)")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger.Info("DEV_MODE: running migrations on startup")
	if err := postgres.RunMigrations(ctx, cfg.DBDSN, migrations.FS); err != nil {
		logger.Error("DEV_MODE: migrations failed on startup", "err", err)
		os.Exit(1)
	}
	if v, _, err := postgres.MigrationsVersion(ctx, cfg.DBDSN); err == nil {
		logger.Info("DEV_MODE: schema version", "version", v)
	} else {
		logger.Warn("DEV_MODE: could not read schema version", "err", err)
	}
}

// bootstrapAdminOnStartup cria o admin inicial se envs
// AIPP_BOOTSTRAP_ADMIN_USERNAME e AIPP_BOOTSTRAP_ADMIN_PASSWORD estão setadas.
// Idempotente: se o usuário já existe (qualquer role), loga e não altera.
//
// Diferente de runBootstrapAdmin (modo CLI), NÃO faz os.Exit em caso de erro
// — apenas loga. O app continua subindo; o admin pode ser criado via CLI
// depois. Motivo: bootstrap é conveniência de dev, não requisito de startup.
func bootstrapAdminOnStartup(cfg *config.Config, logger *slog.Logger, pool *pgxpool.Pool) {
	username := strings.TrimSpace(os.Getenv("AIPP_BOOTSTRAP_ADMIN_USERNAME"))
	if username == "" {
		return
	}
	password := os.Getenv("AIPP_BOOTSTRAP_ADMIN_PASSWORD")
	if password == "" {
		logger.Warn("DEV_MODE: AIPP_BOOTSTRAP_ADMIN_USERNAME set but AIPP_BOOTSTRAP_ADMIN_PASSWORD empty; skipping auto-bootstrap",
			"username", username)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := ensureBootstrapAdmin(ctx, pool, logger, username, password); err != nil {
		logger.Error("DEV_MODE: startup bootstrap-admin failed (use --bootstrap-admin-username CLI to retry)",
			"username", username, "err", err)
	}
}

// ensureBootstrapAdmin garante que existe um usuário admin com o username dado.
// Idempotente: se já existe (qualquer role), NÃO altera — apenas loga e retorna nil.
//
// Extraída para reuso entre o modo CLI (runBootstrapAdmin) e o startup
// automático em DEV_MODE (bootstrapAdminOnStartup).
func ensureBootstrapAdmin(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, username, password string) error {
	if len(username) < 3 || len(username) > 50 {
		return fmt.Errorf("invalid username (must be 3-50 chars): %q", username)
	}
	if len(password) < 12 {
		return fmt.Errorf("password must be at least 12 characters")
	}

	userRepo := postgres.NewUserRepo(pool)

	// Idempotência: se já existe (qualquer role), não altera.
	if existing, err := userRepo.GetByUsername(ctx, domain.DefaultOrganizationID, username); err == nil && existing != nil {
		logger.Info("startup bootstrap-admin: user already exists, skipping",
			"username", username, "current_role", string(existing.Role))
		return nil
	}

	hash, err := domain.HashPassword([]byte(password))
	if err != nil {
		return fmt.Errorf("argon2id hash: %w", err)
	}

	user := &domain.User{
		OrgID:        domain.DefaultOrganizationID,
		Username:     username,
		PasswordHash: []byte(hash),
		Role:         domain.RoleAdmin,
		Status:       domain.UserStatusActive,
	}
	if _, err := userRepo.Create(ctx, user); err != nil {
		return fmt.Errorf("create admin user: %w", err)
	}

	logger.Info("startup bootstrap-admin: admin user created", "username", username)
	return nil
}

// startSessionCleanup dispara limpeza periódica de sessões expiradas.
// Encerra quando o contexto é cancelado (shutdown).
func startSessionCleanup(s domain.SessionStore, interval time.Duration, logger *slog.Logger) {
	// Type-assert para memorySessionStore (única implementação MVP). Se for
	// outro tipo, apenas logamos e não rodamos cleanup (assumimos que o store
	// externo, ex.: Redis, cuida do próprio TTL).
	type cleaner interface{ Cleanup() int }
	c, ok := s.(cleaner)
	if !ok {
		logger.Debug("session store does not support Cleanup; skipping ticker")
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if removed := c.Cleanup(); removed > 0 {
			logger.Info("session cleanup", "removed", removed)
		}
	}
}

// =============================================================================
// Stubs deny-by-default para DEV_MODE sem DB conectado (herdados do F3).
// =============================================================================

func newPoolOrSkip(cfg *config.Config, logger *slog.Logger) *pgxpool.Pool {
	if cfg.DBDSN == "" {
		logger.Warn("DEV_MODE: DATABASE_URL empty — running without DB; auth will deny all requests")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := postgres.NewPool(ctx, cfg)
	if err != nil {
		logger.Error("postgres pool init failed", "err", err)
		os.Exit(1)
	}
	return pool
}

func newTokenValidator(pool *pgxpool.Pool, pepper []byte, logger *slog.Logger) domain.TokenValidator {
	if pool == nil {
		logger.Warn("DEV_MODE: token validator is deny-by-default stub (no DB)")
		return denyAllTokenValidator{}
	}
	return postgres.NewTokenValidator(pool, pepper)
}

func newKeyResolver(pool *pgxpool.Pool, logger *slog.Logger) domain.KeyResolver {
	if pool == nil {
		return denyAllKeyResolver{}
	}
	decryptor := store.NewIdentityDecryptor()
	return postgres.NewKeyResolver(pool, decryptor)
}

// buildRoutingEngine monta o RoutingEngine COMPLETO (F5):
//   - LRUStore (memoryStore) para distribuição de carga.
//   - AffinityStore (memoryStore) para cache de contexto por usuário.
//   - ParserRegistry com DefaultErrorParser para datas de liberação de cota.
//   - AutoDisableManager com threshold 5/janela 2min/backoff crescente.
//   - ReactivationScheduler com tick 30s.
//   - Boot seeding do LRUStore a partir de last_used_at do DB.
//   - Flusher periódico de last_used_at (debounce 60s, spec §3.4.4).
//
// Retorna (engine, scheduler, schedulerCancelFn).
func buildRoutingEngine(cfg *config.Config, logger *slog.Logger, pool *pgxpool.Pool) (
	*routing.RoutingEngine, *routing.ReactivationScheduler, context.CancelFunc,
) {
	// Stores in-memory.
	lruStore := store.NewLRUStore()
	affinityStore := store.NewAffinityStore(
		store.WithAffinityTTL(cfg.AffinityTTL),
		store.WithAffinityMaxPerUser(cfg.AffinityMaxPerUser),
	)

	// Parser registry com defaults para provedores conhecidos.
	parserRegistry := routing.NewParserRegistry()
	routing.RegisterKnownProviders(parserRegistry)

	// Auto-disable manager com config default.
	keyRepo := postgres.NewKeyRepo(pool)
	autoDisable := routing.NewAutoDisableManager(
		routing.DefaultAutoDisableConfig(),
		parserRegistry,
		keyRepo, // implementa routing.KeyRepoPort
		lruStore,
		logger,
	)

	// Engine deps adapter (ModelRepo + KeyRepo).
	modelRepo := postgres.NewModelRepo(pool)
	engineDeps := postgres.NewEngineDepsAdapter(modelRepo, keyRepo)

	// Decryptor: AES-GCM em prod, Identity em dev.
	decryptor := buildDecryptor(cfg, logger)

	// RoutingEngine completo.
	engine := routing.NewRoutingEngine(
		engineDeps, decryptor, lruStore, affinityStore, autoDisable, logger,
	)

	// Boot seeding: popula o LRUStore com last_used_at reais do DB para
	// reconstruir o ranking LRU após restart (spec §3.2.2).
	seedLRUStore(context.Background(), lruStore, keyRepo, domain.OrganizationID(cfg.DefaultOrgID), logger)

	// Scheduler de reativação — tick 30s.
	schedDeps := postgres.NewSchedulerDepsAdapter(keyRepo)
	scheduler := routing.NewReactivationScheduler(
		schedDeps, lruStore,
		domain.OrganizationID(cfg.DefaultOrgID),
		30*time.Second, logger,
	)

	// Context para jobs de background (scheduler + flusher).
	bgCtx, bgCancel := context.WithCancel(context.Background())

	// Flusher de last_used_at: persiste timestamps dirty do LRUStore a cada 60s
	// (debounce, spec §3.4.4). Sem isto, last_used_at fica sempre NULL e o
	// ranking LRU é perdido a cada restart.
	go runLastUsedFlusher(bgCtx, lruStore, keyRepo, domain.OrganizationID(cfg.DefaultOrgID), logger)

	return engine, scheduler, bgCancel
}

// seedLRUStore carrega last_used_at de todas as chaves enabled do DB e popula
// o LRUStore via SeedCandidates. Best-effort: erros são logados mas não fatais
// (o LRUStore será populado lazy via EnsureCandidate a cada request).
func seedLRUStore(ctx context.Context, lru *store.MemoryLRUStore, keyRepo *postgres.KeyRepo, orgID domain.OrganizationID, logger *slog.Logger) {
	entries, err := keyRepo.ListEnabledKeyTimestamps(ctx, orgID)
	if err != nil {
		logger.Warn("LRU boot seed: failed to load key timestamps; store will be populated lazily", "err", err)
		return
	}
	if len(entries) == 0 {
		logger.Info("LRU boot seed: no enabled keys found")
		return
	}
	// Agrupa por provider para chamar SeedCandidates uma vez por provider.
	seedsByProvider := make(map[domain.ID]map[domain.ID]time.Time)
	for _, e := range entries {
		m, ok := seedsByProvider[e.ProviderID]
		if !ok {
			m = make(map[domain.ID]time.Time)
			seedsByProvider[e.ProviderID] = m
		}
		m[e.KeyID] = e.LastUsedAt
	}
	for provID, seeds := range seedsByProvider {
		lru.SeedCandidates(provID, seeds)
	}
	logger.Info("LRU boot seed completed",
		"providers", len(seedsByProvider),
		"total_keys", len(entries),
	)
}

// runLastUsedFlusher persiste timestamps dirty do LRUStore no DB a cada 60s
// (debounce, spec §3.4.4). Termina quando ctx é cancelado (graceful shutdown).
func runLastUsedFlusher(ctx context.Context, lru *store.MemoryLRUStore, keyRepo *postgres.KeyRepo, orgID domain.OrganizationID, logger *slog.Logger) {
	const interval = 60 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Flush final antes de sair (graceful).
			flushLastUsed(lru, keyRepo, logger)
			return
		case <-ticker.C:
			flushLastUsed(lru, keyRepo, logger)
		}
	}
}

// flushLastUsed lê o dirty set do LRUStore e persiste no DB.
func flushLastUsed(lru *store.MemoryLRUStore, keyRepo *postgres.KeyRepo, logger *slog.Logger) {
	dirty := lru.FlushDirty()
	if len(dirty) == 0 {
		return
	}
	if err := keyRepo.TouchLastUsedBatch(context.Background(), dirty); err != nil {
		logger.Warn("last_used_at flush failed; timestamps will be retried next tick", "err", err, "keys", len(dirty))
		return
	}
	logger.Debug("last_used_at flushed", "keys", len(dirty))
}

// buildDecryptor monta o KeyDecryptor conforme o modo:
//   - PROD: AESGCMCrypto com KEK carregada do arquivo (KEK_PATH).
//   - DEV: IdentityDecryptor com AVISO LOUD no log (asssume plaintext no DB).
func buildDecryptor(cfg *config.Config, logger *slog.Logger) domain.KeyDecryptor {
	if cfg.KEKPath != "" {
		kek, err := crypto.LoadKEKFromFile(cfg.KEKPath)
		if err != nil {
			logger.Error("failed to load KEK; falling back to IdentityDecryptor", "err", err)
			return store.NewIdentityDecryptor()
		}
		aes := crypto.NewAESGCMCrypto("v1")
		if err := aes.AddKEK("v1", kek); err != nil {
			logger.Error("failed to add KEK v1; falling back to IdentityDecryptor", "err", err)
			return store.NewIdentityDecryptor()
		}
		logger.Info("AES-256-GCM envelope encryption enabled", "kek_id", "v1")
		return aes
	}
	logger.Warn("DEV_MODE: KEK_PATH not configured; using IdentityDecryptor (plaintext in DB). DO NOT USE IN PRODUCTION.")
	return store.NewIdentityDecryptor()
}

// buildEncryptor monta o KeyEncryptor para o admin (cifragem de chaves ao criar).
// Em prod: AESGCMCrypto (mesma instância do decryptor se possível).
// Em dev: nil (admin guarda plaintext).
func buildEncryptor(cfg *config.Config, logger *slog.Logger) domain.KeyEncryptor {
	if cfg.KEKPath != "" {
		kek, err := crypto.LoadKEKFromFile(cfg.KEKPath)
		if err != nil {
			logger.Error("failed to load KEK for encryptor", "err", err)
			return nil
		}
		aes := crypto.NewAESGCMCrypto("v1")
		if err := aes.AddKEK("v1", kek); err != nil {
			logger.Error("failed to add KEK for encryptor", "err", err)
			return nil
		}
		return aes
	}
	return nil
}

type denyAllTokenValidator struct{}

func (denyAllTokenValidator) Validate(_ context.Context, _ string) (*domain.User, *domain.APIToken, error) {
	return nil, nil, domain.ErrTokenInvalid
}

type denyAllKeyResolver struct{}

func (denyAllKeyResolver) ResolveKey(_ context.Context, _ domain.OrganizationID, _ string) (*domain.KeyResolution, error) {
	return nil, domain.ErrNoKeysAvailable
}

// =============================================================================
// HTTP composeHandler — 3 sub-handlers com prioridade.
// =============================================================================

// composeHandler ordena: Proxy Core (/healthz, /readyz, /metrics, /v1/*) > Admin App
// (/web/*, /api/web/*, /) > Beego (placeholder vazio em F4).
//
// /metrics (F11) só responde se cfg.EnableMetrics=true (registro no mux é gated
// dentro de proxyServer.Handler()). Com EnableMetrics=false, o path cai no
// Beego/admin e retorna 404 — comportamento desejado (endpoint não exposto).
func composeHandler(proxyHandler, adminHandler, beegoHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// 1. Admin API interna (/api/web/*) tem PRIORIDADE sobre o prefixo do
		//    proxy /api/* — evita colisão entre o proxy do cliente e a API da UI.
		if hasPrefix(p, "/api/web/") {
			adminHandler.ServeHTTP(w, r)
			return
		}
		// 2. Proxy Core: health + prefixos /v1 e /api (SDKs OpenAI e coding tools).
		//    O forwarder (forwarder.go stripProxyPrefix) DESCARTA o prefixo e
		//    anexa só o endpoint ao base_url do provedor — a versão (/v1, /v4)
		//    já vive no base_url configurado por provedor.
		if p == "/healthz" || p == "/readyz" || p == "/metrics" ||
			hasPrefix(p, "/v1/") || hasPrefix(p, "/api/") {
			proxyHandler.ServeHTTP(w, r)
			return
		}
		// 3. Admin App: UI /web, /static (assets embed), "/" raiz e convites /invite.
		if hasPrefix(p, "/web/") || p == "/web" ||
			hasPrefix(p, "/static/") ||
			hasPrefix(p, "/invite/") || p == "/invite" ||
			p == "/" {
			adminHandler.ServeHTTP(w, r)
			return
		}
		// 4. Beego: catch-all residual (placeholder em F4; sem rotas reais).
		beegoHandler.ServeHTTP(w, r)
	})
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// writeServiceUnavailable responde 503 simples (para dev sem DB).
func writeServiceUnavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":{"code":"service_unavailable","message":"` + msg + `"}}`))
}

// placeholderBeegoController mantém o Beego "vivo" para F8 (quando templates
// server-rendered reais serão registrados via web.Router). Em F4 é vazio.
type placeholderBeegoController struct {
	web.Controller
}

// buildLogger cria o slog.Logger. JSON em produção, text em dev.
// F7: envolve com SanitizingHandler (redação de chaves/secrets/Authorization).
func buildLogger(cfg *config.Config) *slog.Logger {
	level := parseLevel(cfg.LogLevel)
	return security.BuildSanitizedLogger(cfg.LogFormat, level, os.Stdout)
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func modeStr(dev bool) string {
	if dev {
		return "dev"
	}
	return "prod"
}
