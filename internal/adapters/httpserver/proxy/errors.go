package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// =============================================================================
// Respostas de erro estilo OpenAI.
//
// OpenAI-compatible error format (especificação pública da OpenAI):
//
//	{
//	  "error": {
//	    "message": "...",
//	    "type": "invalid_request_error | authentication_error | rate_limit_error | ...",
//	    "param": null,
//	    "code": "..."
//	  }
//	}
//
// O proxy DEVE devolver erros neste formato para que SDKs OpenAI (Python, JS,
// etc.) consigam deserializá-los. O cliente OpenAI SDK trata erros não-JSON
// como erro genérico de conexão.
// =============================================================================

// openAIError é o envelope de erro compatível com OpenAI.
// "param" é mantido null (não validamos campos individuais no MVP).
type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"` // sempre null no MVP
	Code    string  `json:"code,omitempty"`
}

// ErrorTypes canônicos (subset do que OpenAI usa; suficiente p/ MVP).
const (
	// ErrTypeInvalidRequest erro do cliente (400, 404, 413, 422).
	ErrTypeInvalidRequest = "invalid_request_error"
	// ErrTypeAuthentication token inválido/ausente (401).
	ErrTypeAuthentication = "authentication_error"
	// ErrTypePermission forbidden (403).
	ErrTypePermission = "permission_denied"
	// ErrTypeRateLimit excedeu rate limit (429).
	ErrTypeRateLimit = "rate_limit_error"
	// ErrTypeAPI erro interno do proxy ou do provider (5xx).
	ErrTypeAPI = "api_error"
	// ErrTypeServer provider indisponível (502/503).
	ErrTypeServer = "server_error"
	// ErrTypeUpstream provider retornou erro que repassamos.
	ErrTypeUpstream = "upstream_error"
)

// writeOpenAIError escreve uma resposta de erro no formato OpenAI.
//
// NUNCA incluir dados sensíveis (chaves, tokens, detalhes internos do proxy)
// na mensagem. Mensagens são repassadas ao cliente e logadas no SDK dele.
//
// Para erros internos, prefira mensagens genéricas ("internal error",
// "service unavailable") e logue o detalhe real internamente com request_id.
func writeOpenAIError(w http.ResponseWriter, status int, errType, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := openAIError{
		Error: openAIErrorBody{
			Message: message,
			Type:    errType,
			Param:   nil,
			Code:    code,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}

// Convenience wrappers para os status mais comuns.

func writeAuthError(w http.ResponseWriter, msg string) {
	writeOpenAIError(w, http.StatusUnauthorized, ErrTypeAuthentication, msg, "invalid_api_key")
}

func writeBadRequest(w http.ResponseWriter, msg string) {
	writeOpenAIError(w, http.StatusBadRequest, ErrTypeInvalidRequest, msg, "bad_request")
}

func writeNotFound(w http.ResponseWriter, msg string) {
	writeOpenAIError(w, http.StatusNotFound, ErrTypeInvalidRequest, msg, "not_found")
}

func writeRateLimited(w http.ResponseWriter, msg string, retryAfterSec int) {
	if retryAfterSec > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
	}
	writeOpenAIError(w, http.StatusTooManyRequests, ErrTypeRateLimit, msg, "rate_limit_exceeded")
}

func writeInternalError(w http.ResponseWriter, msg string) {
	writeOpenAIError(w, http.StatusInternalServerError, ErrTypeAPI, msg, "internal_error")
}

func writeBadGateway(w http.ResponseWriter, msg string) {
	writeOpenAIError(w, http.StatusBadGateway, ErrTypeServer, msg, "upstream_unavailable")
}

func writePayloadTooLarge(w http.ResponseWriter) {
	writeOpenAIError(w, http.StatusRequestEntityTooLarge, ErrTypeInvalidRequest,
		"request body exceeds maximum allowed size", "payload_too_large")
}
