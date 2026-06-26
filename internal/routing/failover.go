package routing

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/rafael/ai-pool/internal/domain"
	"github.com/rafael/ai-pool/internal/observability"
)

// =============================================================================
// FailoverChain — orquestrador STATEFUL externo ao pipeline (spec §3.2.3).
//
// Diferença crítica vs uma "policy no pipeline": o failover mantém ESTADO entre
// tentativas (chaves já tentadas, contagem de tentativas, decisão de parar).
// O pipeline de policies (HealthFilter, LRU, Affinity) é stateless por request;
// o FailoverChain INVOCa o pipeline (via RoutingEngine.ResolveKeyWithAffinity)
// múltiplas vezes com `exclude` crescente.
//
// Fluxo:
//
//	triedKeys = []
//	loop:
//	  resolution = engine.ResolveKeyWithAffinity(ctx, ..., exclude=triedKeys)
//	  if resolution == nil (ErrNoKeysAvailable):
//	    return ErrAllKeysExhausted
//	  resp, err = sendUpstream(resolution)
//	  if err (network):
//	    classify = FailureRetryable
//	  else:
//	    classify = ClassifyStatusCode(resp.Status)
//	  switch classify:
//	  case FailureNone:
//	    engine.OnKeySucceeded(...)
//	    copy/stream resposta ao cliente (committed)
//	    return SUCCESS
//	  case FailureClient (400/404/413):
//	    // NÃO há failover — resposta do provedor é repassada ao cliente.
//	    copy resposta (committed)
//	    return SUCCESS_WITH_CLIENT_ERROR
//	  case FailureRetryable (401/403/429/5xx):
//	    // Failover!
//	    if já committed (streaming started):
//	      return ErrFailoverAborted
//	    engine.OnKeyFailed(...)
//	    triedKeys.append(resolution.Key.ID)
//	    backoff.Wait(attempt)
//	    continue
//
// IMPORTANTE — Streaming SSE (spec §3.2.1):
//   - "Committed" = cliente já recebeu pelo menos 1 chunk útil (status 2xx OU
//     qualquer byte da resposta). Após commit, NÃO há failover transparente.
//   - O chain faz UMA decisão crítica por iteração:
//     (a) Pegou resposta. Status não-2xx? → classifica status → failover/client.
//     (b) Status 2xx? → faz streaming; se streaming falha APÓS commit → terminal.
//   - Antes do commit: pode trocar de chave. Após commit: qualquer erro é final.
//
// Backoff (spec §3.2.3):
//   - Exponencial com FULL JITTER (crypto/rand — spec exige).
//   - delay = min(cap, base * 2^attempt) * rand(0,1)
//   - Defaults: base=200ms, cap=5s. Configurável via BackoffConfig.
// =============================================================================

// BackoffConfig parametriza o backoff exponencial entre tentativas de failover.
type BackoffConfig struct {
	Base        time.Duration // default 200ms
	Cap         time.Duration // default 5s
	MaxAttempts int           // default 0 = ilimitado (até esgotar chaves)
}

// DefaultBackoffConfig retorna defaults da spec.
func DefaultBackoffConfig() BackoffConfig {
	return BackoffConfig{
		Base: 200 * time.Millisecond,
		Cap:  5 * time.Second,
	}
}

// Delay calcula o próximo delay com exponential + full jitter (crypto/rand).
// attempt é o número da tentativa falha (0-indexed).
func (b BackoffConfig) Delay(attempt int) time.Duration {
	// exponential: base * 2^attempt, capped.
	exp := time.Duration(int64(b.Base) * int64(math.Pow(2, float64(attempt))))
	if exp > b.Cap || exp <= 0 {
		exp = b.Cap
	}
	// full jitter: rand uniforme em [0, exp).
	jitterInt, _ := randInt63n(int64(exp))
	return time.Duration(jitterInt)
}

// randInt63n retorna um int64 aleatório em [0, n) usando crypto/rand.
// n DEVE ser > 0.
func randInt63n(n int64) (int64, error) {
	if n <= 0 {
		return 0, fmt.Errorf("randInt63n: n must be > 0, got %d", n)
	}
	// Rejection sampling para evitar bias de módulo.
	max := uint64(math.MaxInt64) - (math.MaxInt64 % uint64(n))
	for {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return 0, err
		}
		v := binary.LittleEndian.Uint64(buf[:])
		v &= (1 << 63) - 1 // zera bit de sinal → int64 positivo
		if v < max {
			return int64(v) % n, nil
		}
	}
}

// =============================================================================
// SendUpstreamFunc — porta para envio ao provedor.
// Abstraída para permitir testes com httptest sem depender do http.Client real.
// =============================================================================

// UpstreamOutcome é o resultado de uma tentativa de envio ao upstream.
type UpstreamOutcome struct {
	// Resp é a resposta HTTP recebida (pode ser nil em erro de rede).
	// Caller DEVE fechar resp.Body se não-nil.
	Resp *http.Response
	// Err é erro de rede/io (nil se Resp recebida).
	Err error
}

// SendUpstreamFunc envia um request ao upstream e retorna o outcome.
// Implementação real: http.Client.Do.
type SendUpstreamFunc func(ctx context.Context, req *http.Request) UpstreamOutcome

// =============================================================================
// FailoverResult — resultado de uma execução do chain.
// =============================================================================

// FailoverResult é retornado pelo FailoverChain.Execute.
type FailoverResult struct {
	// Outcome da última tentativa (sucesso OU erro final).
	LastOutcome UpstreamOutcome
	// TriedKeyIDs lista todas as chaves tentadas (para logging/métricas).
	TriedKeyIDs []domain.ID
	// Attempts número total de tentativas.
	Attempts int
	// Committed indica se algo foi escrito ao cliente (não há mais rollback).
	Committed bool
	// Err é o erro final (nil em sucesso, erro em esgotamento/abort).
	Err error
}

// =============================================================================
// FailoverChain — o orquestrador.
// =============================================================================

// FailoverChain orquestra múltiplas tentativas ao upstream em caso de erros
// classificados como retryable.
type FailoverChain struct {
	engine  domain.RoutingEngine
	backoff BackoffConfig
	logger  *slog.Logger
	now     func() time.Time
}

// NewFailoverChain cria o orquestrador.
func NewFailoverChain(
	engine domain.RoutingEngine,
	backoff BackoffConfig,
	logger *slog.Logger,
) *FailoverChain {
	return &FailoverChain{
		engine:  engine,
		backoff: backoff,
		logger:  logger,
		now:     time.Now,
	}
}

// ExecuteOptions parâmetros para uma execução do chain.
type ExecuteOptions struct {
	OrgID           domain.OrganizationID
	UserID          domain.ID
	Model           string
	ConversationKey string
	// MessageCount é o número de mensagens no request atual (usado para
	// AffinityStore.Remember após sucesso).
	MessageCount int
	ProviderSlug string // provider do request atual (para OnKeyFailed/ErrorParser)

	// BuildRequest é callback que monta o http.Request para uma tentativa,
	// dado o KeyResolution selecionado. O caller (proxy) usa isto para
	// construir o request com Authorization, User-Agent, body ORIGINAL etc.
	// O request deve usar o context recebido (que inclui timeouts).
	BuildRequest func(ctx context.Context, resolution *domain.KeyResolution) (*http.Request, error)

	// HandleResponse é callback que processa a resposta do upstream.
	// Deve fazer commit ao cliente (WriteHeader/Write) se necessário e
	// fechar resp.Body. Retorna:
	//   - committed: true se algo foi escrito ao cliente.
	//   - err: erro durante o processamento (nil em sucesso).
	// O callback recebe um context que herda do request principal; se o
	// chain decide fazer failover (ainda não committed), o callback já
	// retornou committed=false e o chain pode prosseguir.
	HandleResponse func(ctx context.Context, resp *http.Response, resolution *domain.KeyResolution) (committed bool, err error)
}

// Execute executa o chain de failover.
//
// Pré-condições:
//   - O body do request JÁ FOI lido e está disponível para BuildRequest.
//   - O contexto recebido inclui o timeout overall (caller é responsável).
//
// Pós-condições:
//   - Em sucesso (FailureNone ou FailureClient): outcome.Resp.Body já fechado
//     por HandleResponse. Cliente recebeu a resposta.
//   - Em esgotamento: retorna FailoverResult.Err = ErrAllKeysExhausted.
//   - Em abort (committed then error): retorna FailoverResult.Err com Committed=true.
func (fc *FailoverChain) Execute(
	ctx context.Context,
	opts ExecuteOptions,
	send SendUpstreamFunc,
) FailoverResult {
	var (
		triedKeys []domain.ID
		attempts  int
	)

	for {
		// Limite de tentativas (se configurado) — conta apenas tentativas que
		// realmente enviaram ao upstream (não iterações que falharam em resolver).
		if fc.backoff.MaxAttempts > 0 && attempts >= fc.backoff.MaxAttempts {
			fc.logger.Warn("failover: max attempts reached",
				"attempts", attempts, "tried", len(triedKeys))
			return FailoverResult{
				TriedKeyIDs: triedKeys,
				Attempts:    attempts,
				Err:         ErrAllKeysExhausted,
			}
		}

		// 1. Resolve próxima chave (excluindo já tentadas).
		resolution, err := fc.engine.ResolveKeyWithAffinity(
			ctx, opts.OrgID, opts.UserID, opts.Model,
			opts.ConversationKey, triedKeys,
		)
		if err != nil {
			if errors.Is(err, domain.ErrNoKeysAvailable) || errors.Is(err, domain.ErrModelNotFound) {
				fc.logger.Info("failover: no more keys",
					"attempts", attempts, "tried", len(triedKeys),
					"err", err)
				return FailoverResult{
					TriedKeyIDs: triedKeys,
					Attempts:    attempts,
					Err:         ErrAllKeysExhausted,
				}
			}
			// Erro inesperado do engine (DB, decrypt).
			return FailoverResult{
				TriedKeyIDs: triedKeys,
				Attempts:    attempts,
				Err:         fmt.Errorf("engine resolve: %w", err),
			}
		}

		// Resolução OK — incrementa contador de tentativas (uma tentativa ao upstream).
		attempts++

		// === Instrumentação Prometheus (F11) ===
		// Uma tentativa ao upstream = um inc em failover_attempts_total.
		// ProviderSlug pode estar vazio (o caller/wrapper ainda não preencheu);
		// o helper sanitiza para "unknown" — aceitável; provider real é
		// correlacionável via proxy_requests_total (mesma request).
		observability.IncFailoverAttempt(opts.ProviderSlug)

		// 2. Monta request ao upstream com a chave selecionada.
		req, err := opts.BuildRequest(ctx, resolution)
		if err != nil {
			// Erro de construção é terminal (provavelmente body inválido etc.).
			return FailoverResult{
				TriedKeyIDs: triedKeys, Attempts: attempts, Err: fmt.Errorf("build request: %w", err),
			}
		}

		// 3. Envia ao upstream.
		outcome := send(ctx, req)

		// 4. Classifica o resultado.
		//    (a) Erro de rede/io → retryable.
		//    (b) Status HTTP recebido → classifica status.
		var kind FailureKind
		var statusCode int
		if outcome.Err != nil {
			kind = FailureRetryable
			fc.logger.Debug("failover: upstream network error",
				"key_id", resolution.Key.ID, "err", outcome.Err)
		} else if outcome.Resp != nil {
			statusCode = outcome.Resp.StatusCode
			kind = ClassifyStatusCode(statusCode)
		} else {
			// Inconsistência: nem erro nem resp. Trata como terminal.
			return FailoverResult{
				TriedKeyIDs: triedKeys, Attempts: attempts,
				Err: errors.New("upstream returned neither response nor error"),
			}
		}

		// 5. Decisão por categoria.
		switch kind {
		case FailureNone:
			// Sucesso (2xx). Processa resposta (commit ao cliente).
			committed, herr := opts.HandleResponse(ctx, outcome.Resp, resolution)
			if outcome.Resp != nil && outcome.Resp.Body != nil {
				_, _ = io.Copy(io.Discard, outcome.Resp.Body)
				outcome.Resp.Body.Close()
			}
			if herr != nil {
				// Erro durante streaming do sucesso. Se committed, terminal.
				_ = fc.engine.OnKeyFailed(ctx, opts.OrgID, resolution.Key.ID,
					resolution.Provider.ID, opts.ProviderSlug, statusCode, nil)
				return FailoverResult{
					LastOutcome: outcome, TriedKeyIDs: triedKeys, Attempts: attempts,
					Committed: committed, Err: fmt.Errorf("handle success response: %w", herr),
				}
			}
			// Sucesso real — notifica engine.
			_ = fc.engine.OnKeySucceeded(ctx, opts.OrgID, opts.UserID,
				opts.ConversationKey, resolution.Key.ID, opts.MessageCount)
			return FailoverResult{
				LastOutcome: outcome, TriedKeyIDs: triedKeys, Attempts: attempts,
				Committed: committed,
			}

		case FailureClient:
			// 400/404/413 — resposta do provedor É repassada ao cliente.
			// Não há failover (erro é do cliente, mesmo com outra chave).
			committed, herr := opts.HandleResponse(ctx, outcome.Resp, resolution)
			if outcome.Resp != nil && outcome.Resp.Body != nil {
				_, _ = io.Copy(io.Discard, outcome.Resp.Body)
				outcome.Resp.Body.Close()
			}
			if herr != nil && !committed {
				return FailoverResult{
					LastOutcome: outcome, TriedKeyIDs: triedKeys, Attempts: attempts,
					Err: fmt.Errorf("handle client error response: %w", herr),
				}
			}
			return FailoverResult{
				LastOutcome: outcome, TriedKeyIDs: triedKeys, Attempts: attempts,
				Committed: true, // resposta foi repassada (committed implicitamente)
			}

		case FailureRetryable:
			// 401/403/429/5xx/network — FAIL OVER.
			// Captura body para AutoDisableManager.ErrorParser (best-effort).
			var errBody []byte
			if outcome.Resp != nil {
				if outcome.Resp.Body != nil {
					// Lê até 4KB para o parser (evita ler body gigante).
					errBody, _ = io.ReadAll(io.LimitReader(outcome.Resp.Body, 4096))
					outcome.Resp.Body.Close()
				}
			}
			// Registra erro (pode disparar auto-disable).
			_ = fc.engine.OnKeyFailed(ctx, opts.OrgID, resolution.Key.ID,
				resolution.Provider.ID, opts.ProviderSlug, statusCode, errBody)

			triedKeys = append(triedKeys, resolution.Key.ID)

			// Backoff antes da próxima tentativa (respeitando cancelamento).
			delay := fc.backoff.Delay(attempts - 1)
			select {
			case <-ctx.Done():
				return FailoverResult{
					LastOutcome: outcome, TriedKeyIDs: triedKeys, Attempts: attempts,
					Err: ctx.Err(),
				}
			case <-time.After(delay):
			}
			continue

		case FailureStreamCommitted:
			// Erro após commit — terminal. Cliente recebeu parte da resposta.
			return FailoverResult{
				LastOutcome: outcome, TriedKeyIDs: triedKeys, Attempts: attempts,
				Committed: true,
				Err:       ErrFailoverAborted,
			}
		}
	}
}
