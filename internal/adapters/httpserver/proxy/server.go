// Package proxy implementa o Proxy Core OpenAI-compatible.
//
// PRINCÍPIO (spec §3.1.2, ADR-007): net/http PURO com http.Flusher para
// streaming SSE. NÃO usa Beego (que pode bufferizar e quebrar SSE no hot path).
//
// Funcionamento (F3):
//
//	Cliente → AuthMiddleware (DB hit, Bearer sk-aipp-...)
//	        → handleV1:
//	            1. Lê body original em []byte (preserva bytes).
//	            2. Parse parcial (model, stream) — sem re-serializar.
//	            3. keyResolver.ResolveKey(model) → provider + key + decrypted.
//	               (F3: primeira chave usable. F5: LRU + afinidade + failover.)
//	            4. buildUpstreamRequest (auth+UA+body original).
//	            5. sendUpstream com context (overall timeout).
//	            6. handleUpstreamResponse → copyUpstreamResponse OU streamSSE
//	               (3 timeouts: first-byte, idle, overall).
//	            7. Resp.Body.Close().
//
// Importante: erros do provedor (status não-2xx) são REPASSADOS ao cliente
// no formato original, para que SDKs OpenAI tratem corretamente (ex.: 429
// dispara retry automático no SDK). Erros do proxy (auth, roteamento) são
// devolvidos no formato OpenAI.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hacklabr/ai-pool/internal/config"
	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/observability"
)

// Server é o Proxy Core. Carrega todas as dependências via struct fields
// (DI friendly; facilita testes com mocks).
type Server struct {
	tokenValidator domain.TokenValidator
	keyResolver    domain.KeyResolver
	upstreamClient *http.Client
	logger         *slog.Logger
	cfg            *config.Config
	streamCfg      streamConfig
	maxBodyBytes   int64
	// failoverChain (opcional, F5). Se nil, cai para o fluxo simples sem
	// failover (compatível com testes F3 e setups onde o routing engine não
	// está conectado).
	failoverChain FailoverExecuter
	// rateLimiter (opcional, F6). Se nil, sem rate limiting.
	rateLimiter domain.RateLimiter
	// usageCollector (opcional, F6). Se nil, não registra uso.
	usageCollector domain.UsageCollector
}

// FailoverExecuter é a porta mínima do orquestrador de failover que o proxy
// usa. Definida aqui (não importando routing.FaiLoverChain diretamente) para
// manter o proxy desacoplado do pacote routing e facilitar mocks.
//
// Em F5 a implementação concreta é um wrapper em torno de routing.FailoverChain
// (criado no main.go).
type FailoverExecuter interface {
	// ExecuteProxyRequest orquestra: resolve chave → monta request → envia →
	// trata resposta (com failover se necessário).
	//
	// Retorna:
	//   - committed=true + err=nil: resposta entregue ao cliente com sucesso.
	//   - committed=true + err!=nil: resposta parcial entregue (stream abortado).
	//   - committed=false + err!=nil: nenhum byte escrito; proxy decide resposta.
	ExecuteProxyRequest(
		ctx context.Context,
		w http.ResponseWriter,
		clientReq *http.Request,
		bodyBytes []byte,
		spec upstreamRequestSpec,
		user *domain.User,
		conversationID string,
	) (committed bool, finalErr error)
}

// NewServer cria o Proxy Core com todas as dependências.
//
// Caller (main.go) é responsável por:
//   - Construir o tokenValidator com pepper carregado (env/file).
//   - Construir o keyResolver com decryptor apropriado (Identity dev, AES-GCM prod).
//   - Construir o upstreamClient (via newUpstreamClient(cfg)).
//   - Validar SSRF dos providers cadastrados no boot (em F7, no admin path).
//   - Opcionalmente injetar failoverChain (F5+) para habilitar failover/LRU/afinidade.
func NewServer(
	cfg *config.Config,
	logger *slog.Logger,
	tokenValidator domain.TokenValidator,
	keyResolver domain.KeyResolver,
	upstreamClient *http.Client,
) *Server {
	return &Server{
		tokenValidator: tokenValidator,
		keyResolver:    keyResolver,
		upstreamClient: upstreamClient,
		logger:         logger,
		cfg:            cfg,
		streamCfg:      streamConfigFromCfg(cfg),
		maxBodyBytes:   cfg.RequestBodyMaxBytes,
	}
}

// SetFailoverChain injeta o orquestrador de failover (F5+).
// Se nil, o proxy usa fluxo simples sem failover (compat F3).
func (s *Server) SetFailoverChain(fc FailoverExecuter) {
	s.failoverChain = fc
}

// SetRateLimiter injeta o rate limiter (F6). Se nil, sem rate limiting.
func (s *Server) SetRateLimiter(rl domain.RateLimiter) {
	s.rateLimiter = rl
}

// SetUsageCollector injeta o coletor de uso (F6). Se nil, não registra.
func (s *Server) SetUsageCollector(uc domain.UsageCollector) {
	s.usageCollector = uc
}

// RateLimiter expõe o limiter para uso em outros pacotes (ex.: admin login).
// Pode retornar nil se não configurado.
func (s *Server) RateLimiter() domain.RateLimiter { return s.rateLimiter }

// UsageCollector expõe o coletor para uso em outros pacotes (ex.: failover wrapper).
// Pode retornar nil se não configurado.
func (s *Server) UsageCollector() domain.UsageCollector { return s.usageCollector }

// Handler retorna o http.Handler para o Proxy Core.
//
// Rotas:
//   - GET  /healthz  → liveness (processo vivo).
//   - GET  /readyz   → readiness (delega a checker externo).
//   - ANY  /v1/*     → AuthMiddleware → handleV1 (repasse real).
//
// O handler retornado é montado no http.Server subjacente (NÃO no mux do
// Beego) para garantir controle de http.Flusher no streaming SSE.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz)

	// /metrics (F11): endpoint Prometheus GATEADO por config.EnableMetrics.
	// Spec §3.5.4: default false no MVP (instrumentação sempre ativa, só o
	// endpoint é gated). Quando Prometheus + Grafana chegarem ao cluster,
	// basta setar ENABLE_METRICS=true para expor /metrics no ServiceMonitor.
	if s.cfg.EnableMetrics {
		mux.Handle("GET /metrics", promhttp.Handler())
	}

	// /v1/* e /api/* com middleware de auth (DB hit por request).
	// Ambos os prefixos são aceitos: /v1 (SDKs OpenAI) e /api (coding tools).
	// O forwarder (stripProxyPrefix) descarta o prefixo antes de montar a URL
	// upstream — a versão (/v1,/v4) já vive no base_url do provedor.
	v1Handler := s.AuthMiddleware(http.HandlerFunc(s.handleV1))
	mux.Handle("/v1/", v1Handler)
	mux.Handle("/api/", v1Handler)

	return mux
}

// handleHealthz retorna 200 se o processo está vivo. NÃO checa DB.
// Liveness probe do K8s aponta aqui (spec §3.5.2).
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleReadyz retorna 200 quando deps (DB, etc.) estão prontos.
// Em F3, ainda não checamos DB aqui (F2 já faz ping no boot). Melhoria em F6.
func handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

// handleV1 é o handler PRINCIPAL do proxy: recebe requisição OpenAI-compatible
// e repassa ao provedor preservando o body original.
//
// Erros conhecidos (status HTTP + formato OpenAI):
//   - 401: token inválido/ausente (já tratado pelo AuthMiddleware).
//   - 400: body inválido (não-JSON ou sem campo "model").
//   - 413: body excede max.
//   - 404: model não encontrado em nenhum provider ativo.
//   - 503: provider não tem chaves usable (ErrNoKeysAvailable).
//   - 502: provedor indisponível (rede/timeout).
//   - 5xx do provedor: repassado ao cliente no formato original.
//   - 2xx do provedor: repassado (com streaming SSE se aplicável).
func (s *Server) handleV1(w http.ResponseWriter, r *http.Request) {
	// Timer para métrica de duration (F11). Observado no fim de cada path.
	// Capturamos spec.Model e providerSlug (resolvido posteriormente) para
	// a label; default "unknown" se não resolvido.
	startTime := time.Now()
	var (
		finalModel    string
		finalProvider string
	)
	// observeDuration helper para não repetir em cada return.
	observeDuration := func() {
		observability.ObserveProxyDuration(finalProvider, finalModel, time.Since(startTime).Seconds())
	}

	// 1. Lê body original preservando bytes.
	bodyBytes, err := readBodyAndRestore(r, s.maxBodyBytes)
	if err != nil {
		// Distingue "too large" (413) de outros erros de IO (400).
		if isTooLarge(err) {
			observability.IncProxyRequest("", "", http.StatusRequestEntityTooLarge)
			writePayloadTooLarge(w)
			return
		}
		observability.IncProxyRequest("", "", http.StatusBadRequest)
		writeBadRequest(w, "failed to read request body")
		return
	}

	// Endpoints sem body (ex.: GET /v1/models): repassa sem parse de model.
	// Para estes, "model" é derivado do path. Por ora, exigimos body JSON para
	// endpoints de chat/completions. F5 tratará /v1/models sem model.
	if len(bodyBytes) == 0 {
		observability.IncProxyRequest("", "", http.StatusBadRequest)
		writeBadRequest(w, "request body is required")
		return
	}

	// 2. Parse parcial: extrai model e stream para decisões de roteamento.
	spec, err := parseRequestSpec(bodyBytes)
	if err != nil {
		observability.IncProxyRequest("", "", http.StatusBadRequest)
		writeBadRequest(w, "invalid JSON body")
		return
	}
	if spec.Model == "" {
		observability.IncProxyRequest("", "", http.StatusBadRequest)
		writeBadRequest(w, `missing required field: "model"`)
		return
	}
	finalModel = spec.Model

	// Usuário autenticado (populado pelo AuthMiddleware).
	user := userFromContext(r.Context())
	if user == nil {
		// Não deveria acontecer — AuthMiddleware garante. Programmatic error.
		observability.IncProxyRequest("", finalModel, http.StatusInternalServerError)
		writeInternalError(w, "internal: no user in context")
		return
	}

	s.logger.Debug("proxy request",
		"path", r.URL.Path,
		"model", spec.Model,
		"stream", spec.IsStream,
		"user_id", strconv.FormatInt(int64(user.ID), 10),
		"org_id", strconv.FormatInt(int64(user.OrgID), 10),
	)

	// === RATE LIMITING (F6) ===
	// Limite por token (escopo = "token:<token_id>"). Aplicado ANTES do forward.
	// Se excedido → 429 + Retry-After. NÃO registra no usage_log como sucesso.
	if s.rateLimiter != nil {
		tok := tokenFromContext(r.Context())
		scope := "token:0" // fallback se token somehow nil
		if tok != nil {
			scope = "token:" + strconv.FormatInt(int64(tok.ID), 10)
		}
		allowed, retryAfter, err := s.rateLimiter.Allow(r.Context(), scope)
		if err != nil {
			s.logger.Warn("rate limiter error (allowing)", "err", err)
		} else if !allowed {
			retrySec := int(retryAfter.Seconds())
			if retrySec < 1 {
				retrySec = 1
			}
			writeRateLimited(w, "rate limit exceeded for this API token", retrySec)
			// Registra o 429 no usage log (para auditoria do rate-limit hit).
			s.recordUsage(user, tok, spec, 0, 0, 0, 0, http.StatusTooManyRequests,
				ptrString("rate_limit"), 0, "")
			// recordUsage já inc'd a métrica de requests_total + tokens (=0, no-op).
			observeDuration()
			return
		}
	}

	// === CAMINHO COM FAILOVER (F5+) ===
	// Se o failover chain está injetado, delega para ele toda a lógica de
	// resolução + envio + failover + tratamento de resposta.
	if s.failoverChain != nil {
		// Extrai ID de sessão/conversa (X-Session-Id do OpenCode, ou X-Conversation-ID).
		convID := ConversationIDFromRequest(r)

		// Context com overall timeout (herdado do request do cliente).
		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UpstreamOverallTimeout)
		defer cancel()

		committed, err := s.failoverChain.ExecuteProxyRequest(
			ctx, w, r, bodyBytes, spec, user, convID,
		)
		if err != nil {
			s.logger.Warn("failover chain completed with error",
				"committed", committed, "err", err)
			if !committed {
				// Ainda dá tempo de responder erro ao cliente.
				writeBadGateway(w, "all keys failed or unavailable")
				// Métrica de erro terminal do chain. recordUsage não é chamado
				// aqui (wrapper não tem tokens); registramos só requests_total.
				observability.IncProxyRequest("", finalModel, http.StatusBadGateway)
			}
		}
		observeDuration()
		return
	}

	// === CAMINHO SIMPLES (compat F3, sem failover) ===
	// Usado quando failoverChain é nil (setup mínimo, testes unitários).
	// Resolve provider+key+decrypted.
	resolution, err := s.keyResolver.ResolveKey(r.Context(), user.OrgID, spec.Model)
	if err != nil {
		s.handleResolverError(w, err, spec.Model)
		// Métrica do erro de resolução. recordUsage não é chamado neste path
		// (não há tokens); registramos só requests_total com status apropriado.
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, domain.ErrModelNotFound):
			status = http.StatusNotFound
		case errors.Is(err, domain.ErrNoKeysAvailable):
			status = http.StatusBadGateway
		}
		observability.IncProxyRequest("", finalModel, status)
		observeDuration()
		return
	}
	finalProvider = resolution.Provider.Slug
	// Defensivo: zeroiza o segredo decifrado ao final (limpa memória).
	defer zeroBytes(resolution.DecryptedValue)

	s.logger.Debug("key resolved",
		"provider", resolution.Provider.Slug,
		"key_id", strconv.FormatInt(int64(resolution.Key.ID), 10),
		"provider_ua_override", resolution.Provider.UserAgentGlobal != nil,
		"key_ua_override", resolution.Key.UserAgentOverride != nil,
	)

	// 4. Monta request ao upstream (auth+UA+body ORIGINAL).
	upReq, err := buildUpstreamRequest(r, bodyBytes, resolution, spec.IsStream)
	if err != nil {
		s.logger.Error("build upstream request", "err", err)
		observability.IncProxyRequest(finalProvider, finalModel, http.StatusInternalServerError)
		writeInternalError(w, "internal error")
		observeDuration()
		return
	}

	// 5. Envia ao upstream. Context já herda do cliente (cancelamento em
	// desconexão é automático). Overall timeout aplicado aqui.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UpstreamOverallTimeout)
	defer cancel()

	resp, err := sendUpstream(ctx, s.upstreamClient, upReq)
	if err != nil {
		// Erro de rede/dial/timeout. Em F3, terminal (F5 pode failover).
		s.logger.Warn("upstream send failed",
			"err", err,
			"provider", resolution.Provider.Slug,
		)
		observability.IncProxyRequest(finalProvider, finalModel, http.StatusBadGateway)
		writeBadGateway(w, "upstream provider unavailable")
		observeDuration()
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// 6. Processa resposta: copy simples OU streaming SSE com 3 timeouts.
	if resolution.ViaAffinity {
		w.Header().Set("X-Context-Cache", "HIT")
	} else {
		w.Header().Set("X-Context-Cache", "MISS")
	}
	result := s.handleUpstreamResponse(w, r.Context(), resp, spec.IsStream, s.streamCfg)
	if result.Err != nil {
		// Se já committed, não podemos mudar a resposta — apenas logar.
		// Cliente recebeu resposta parcial ou um chunk de erro.
		s.logger.Warn("response handling completed with error",
			"committed", result.Committed,
			"status", result.StatusCode,
			"err", result.Err,
		)
		// Se NÃO committed e há erro, devolvemos 502 (ainda dá tempo).
		if !result.Committed {
			observability.IncProxyRequest(finalProvider, finalModel, http.StatusBadGateway)
			writeBadGateway(w, "upstream stream failed before any data")
		}
	}
	// Registra uso (tokens do campo usage do provedor). No caminho simples,
	// provider/key ids ficam 0 (caminho de testes; produção usa o failover
	// wrapper, que preenche via RecordProxyUsage).
	statusForLog := result.StatusCode
	if statusForLog == 0 {
		statusForLog = http.StatusOK
	}
	s.recordUsage(user, tokenFromContext(r.Context()), spec,
		result.PromptTokens, result.CompletionTokens, result.CachedTokens, 0,
		statusForLog, nil, time.Since(startTime).Milliseconds(), finalProvider)
	// Latência total — observa independentemente de sucesso/erro (a label
	// model+provider já foi resolvida neste ponto do caminho simples).
	observeDuration()
}

// handleResolverError mapeia erros do KeyResolver para respostas HTTP apropriadas.
func (s *Server) handleResolverError(w http.ResponseWriter, err error, model string) {
	switch {
	case errors.Is(err, domain.ErrModelNotFound):
		writeNotFound(w, "model '"+model+"' is not available in any active provider")
	case errors.Is(err, domain.ErrNoKeysAvailable):
		writeBadGateway(w, "no usable API keys for model '"+model+"'")
	case errors.Is(err, domain.ErrKeyDecryptNotConfigured):
		// Programmatic error — produção sem decryptor injetado.
		s.logger.Error("decryptor not configured", "err", err)
		writeInternalError(w, "internal error")
	default:
		s.logger.Error("key resolver unexpected error", "err", err)
		writeInternalError(w, "internal error")
	}
}

// isTooLarge detecta erro de "body too large" do http.MaxBytesReader.
// O erro retornado é *http.MaxBytesError no Go 1.19+.
func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// zeroBytes preenche o slice com zeros (defesa contra exposição em dumps).
// Não é garantia forte (Go GC pode ter cópias), mas reduz a janela.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
