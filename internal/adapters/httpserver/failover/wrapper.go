// Package failover integra o routing.FailoverChain com o proxy.Server.
//
// Mantém o pacote proxy desacoplado do pacote routing (proxy não importa routing
// diretamente). O main.go instancia este wrapper e injeta via SetFailoverChain.
package failover

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/httpserver/proxy"
	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/routing"
)

// ChainWrapper implementa proxy.FailoverExecuter usando routing.FailoverChain.
//
// Responsabilidades:
//   - Computa conversationKey a partir de X-Conversation-ID + hash do prefixo
//     das mensagens (afinidade de cache de contexto).
//   - Fornece ao chain os callbacks BuildRequest e HandleResponse que usam
//     funções do pacote proxy (exportadas para este fim).
//   - Fornece SendUpstreamFunc usando o http.Client injetado.
type ChainWrapper struct {
	engine         domain.RoutingEngine
	chain          *routing.FailoverChain
	server         *proxy.Server
	upstreamClient *http.Client
}

// NewChainWrapper cria o wrapper.
//
// engine:           routing engine completo (LRU + afinidade + auto-disable).
// server:           proxy.Server (para acesso ao HandleUpstreamResponseExported
//
//	com timeouts configurados).
//
// upstreamClient:   http.Client com pooling (proxy.NewUpstreamClient).
// backoff:          configuração de backoff entre tentativas.
// logger:           logger estruturado.
func NewChainWrapper(
	engine domain.RoutingEngine,
	server *proxy.Server,
	upstreamClient *http.Client,
	backoff routing.BackoffConfig,
	logger *slog.Logger,
) *ChainWrapper {
	return &ChainWrapper{
		engine:         engine,
		chain:          routing.NewFailoverChain(engine, backoff, logger),
		server:         server,
		upstreamClient: upstreamClient,
	}
}

// ExecuteProxyRequest implementa proxy.FailoverExecuter.
//
// Fluxo:
//  1. Extrai messages do body (parse parcial) e computa conversationKey
//     (X-Conversation-ID preferido; hash do prefixo fallback).
//  2. Chama chain.Execute com callbacks apropriados.
//  3. Traduz o resultado para (committed, err).
func (w *ChainWrapper) ExecuteProxyRequest(
	ctx context.Context,
	respWriter http.ResponseWriter,
	clientReq *http.Request,
	bodyBytes []byte,
	spec proxy.UpstreamRequestSpec,
	user *domain.User,
	conversationID string,
) (bool, error) {
	start := time.Now()
	// 1. Computa conversationKey (afinidade). Se há ID de sessão via header
	// (OpenCode envia X-Session-Id), usamos ele direto e PULAMOS o hash do body
	// (pode ser 188KB+ — hashing seria desperdício, e o ID do header é mais estável).
	msgs, _ := routing.ExtractMessages(bodyBytes)
	messageCount := len(msgs)
	var prefixHash string
	if conversationID == "" {
		prefixHash, _ = routing.HashPrefix(msgs)
	}
	conversationKey := routing.ResolveConversationKey(conversationID, prefixHash)

	// 2. ProviderSlug — o engine resolve o provider INTERNAMENTE por tentativa;
	// passamos vazio aqui e o chain usa o slug real da resolução
	// (routing.attemptSlug) para OnKeyFailed/ErrorParser/ClassifyLimitLayer
	// (issue #19, de-quebra: parsing provider-aware).
	providerSlug := ""

	// 3. Executa o chain.
	result := w.chain.Execute(ctx, routing.ExecuteOptions{
		OrgID:           user.OrgID,
		UserID:          user.ID,
		Model:           spec.Model,
		ConversationKey: conversationKey,
		MessageCount:    messageCount,
		ProviderSlug:    providerSlug,
		BuildRequest: func(_ context.Context, resolution *domain.KeyResolution) (*http.Request, error) {
			// proxy.BuildUpstreamRequest constrói o request ao upstream com a
			// chave resolved (auth substituída, UA override, body ORIGINAL).
			return proxy.BuildUpstreamRequest(clientReq, bodyBytes, resolution, spec.IsStream)
		},
		HandleResponse: func(hctx context.Context, hr *http.Response, resolution *domain.KeyResolution) (bool, error) {
			// Header de resposta X-Context-Cache: HIT (afinidade → mesma chave da
			// sessão, possível cache do provedor) ou MISS (LRU/rotação). Setado
			// ANTES de escrever a resposta.
			if resolution != nil {
				if resolution.ViaAffinity {
					respWriter.Header().Set("X-Context-Cache", "HIT")
				} else {
					respWriter.Header().Set("X-Context-Cache", "MISS")
				}
			}
			// server.HandleUpstreamResponseExported faz copy OU streaming SSE
			// usando os timeouts configurados no Server. resp.Body é fechado
			// pelo chain após HandleResponse.
			r := w.server.HandleUpstreamResponseExported(respWriter, hctx, hr, spec.IsStream)
			// Registra o uso da resposta FINAL (o chain chama HandleResponse
			// no máximo uma vez — só na tentativa que responde ao cliente).
			var providerSlug string
			var providerID, keyID int64
			if resolution != nil {
				providerSlug = resolution.Provider.Slug
				providerID = int64(resolution.Provider.ID)
				keyID = int64(resolution.Key.ID)
			}
			// error_class para falhas upstream (issue #19, de-quebra b):
			// 429→quota/rate_limit (via ClassifyLimitLayer quando há corpo),
			// 401/403→auth, 5xx→5xx; nil em sucesso/erro do cliente.
			errorClass := routing.ClassifyErrorClass(r.StatusCode, providerSlug, "")
			w.server.RecordProxyUsage(user, spec,
				r.PromptTokens, r.CompletionTokens, r.CachedTokens, r.StatusCode,
				time.Since(start).Milliseconds(), errorClass, providerSlug, providerID, keyID)
			return r.Committed, r.Err
		},
	}, w.sendFunc())

	return result.Committed, result.Err
}

// sendFunc retorna uma SendUpstreamFunc que usa o http.Client injetado.
// O context é injetado pelo chain em cada req.
func (w *ChainWrapper) sendFunc() routing.SendUpstreamFunc {
	return func(ctx context.Context, req *http.Request) routing.UpstreamOutcome {
		req = req.WithContext(ctx)
		// #nosec G704 -- o ChainWrapper só repassa requests cuja URL já foi
		// validada por domain.ValidateProviderURL no cadastro do provider.
		// O failover itera sobre chaves do mesmo provider, não constrói URLs.
		r, err := w.upstreamClient.Do(req)
		if err != nil {
			return routing.UpstreamOutcome{Resp: nil, Err: err}
		}
		return routing.UpstreamOutcome{Resp: r, Err: nil}
	}
}

// silence unused import warning (io é reservado para uso futuro).
var _ = io.Discard
