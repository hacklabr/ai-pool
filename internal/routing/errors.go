package routing

import (
	"errors"
	"net/http"
)

// =============================================================================
// ErrorClassifier — classifica status codes / erros em categorias de ação.
//
// Spec §3.2.3:
//   - NÃO disparam failover: 400 (bad request — problema do cliente), 404,
//     413 (payload grande). Erro do cliente, não adianta tentar outra chave.
//   - DISPARAM failover: 401/403 (auth — embora possa indicar chave inválida),
//     429 (rate limit), 5xx (erro do servidor), erros de rede/timeout.
//
// "Disparam failover" significa: tentar a próxima chave habilitada do mesmo
// provider (FailoverChain). Se já houve Committed (cliente recebeu parte da
// resposta), não há failover transparente — propaga o erro.
// =============================================================================

// FailureKind classifica o resultado de uma tentativa ao upstream.
type FailureKind int

const (
	// FailureNone sucesso (2xx completo ou streaming finalizado sem erro).
	FailureNone FailureKind = iota

	// FailureClient erro do cliente (400/404/413/422). NÃO dispara failover.
	// A resposta do provedor DEVE ser repassada ao cliente (SDK OpenAI precisa
	// ver o erro real para retry/tratamento apropriado).
	FailureClient

	// FailureRetryable erro que JUSTIFICA tentar outra chave (401/403/429/5xx/
	// network/timeout). FailoverChain tenta a próxima habilitada.
	FailureRetryable

	// FailureStreamCommitted erro ocorreu APÓS o primeiro chunk útil do stream.
	// Não há failover transparente — cliente já recebeu parte da resposta.
	// Resposta é terminal (propagar fechamento).
	FailureStreamCommitted
)

// ClassifyStatusCode classifica um status HTTP do upstream em FailureKind.
// Para erros de rede (sem status), usar ClassifyError.
func ClassifyStatusCode(statusCode int) FailureKind {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return FailureNone
	case statusCode == http.StatusBadRequest, statusCode == http.StatusNotFound,
		statusCode == http.StatusRequestEntityTooLarge, statusCode == http.StatusUnprocessableEntity,
		statusCode == http.StatusUnsupportedMediaType, statusCode == http.StatusNotImplemented:
		// Erro do cliente — não adianta trocar de chave, erro é o mesmo.
		return FailureClient
	case statusCode == http.StatusUnauthorized, statusCode == http.StatusForbidden,
		statusCode == http.StatusTooManyRequests,
		statusCode == http.StatusRequestTimeout,
		statusCode == http.StatusConflict, // alguns provedores usam 409 p/ rate limit
		statusCode >= 500 && statusCode < 600:
		// Erro potencialmente transitório ou de chave — tentar outra.
		return FailureRetryable
	default:
		// Status desconhecido (1xx, 3xx c/ código estranho): conservadoramente
		// tratar como retryable (provedor pode estar com comportamento estranho).
		return FailureRetryable
	}
}

// ClassifyError classifica um error de rede/io (sem status HTTP disponível).
// Timeouts e connection refused → retryable.
func ClassifyError(err error) FailureKind {
	if err == nil {
		return FailureNone
	}
	// Erros de rede/timeout são retryable por padrão.
	// Não distinguimos tipos específicos aqui (evitar import de net/*);
	// a regra é: erro antes de receber status = retryable.
	return FailureRetryable
}

// IsRetryable combina ClassifyStatusCode e ClassifyError para conveniência.
// Útil no FailoverChain.
func IsRetryable(statusCode int, err error) bool {
	if err != nil {
		return ClassifyError(err) == FailureRetryable
	}
	return ClassifyStatusCode(statusCode) == FailureRetryable
}

// ErrAllKeysExhausted é retornado pelo FailoverChain quando TODAS as chaves
// habilitadas do provider foram tentadas e nenhuma teve sucesso.
var ErrAllKeysExhausted = errors.New("all keys exhausted in failover chain")

// ErrFailoverAborted é retornado quando o failover não pode prosseguir
// (ex.: resposta já committed ao cliente, não há rollback possível).
var ErrFailoverAborted = errors.New("failover aborted (response already committed)")
