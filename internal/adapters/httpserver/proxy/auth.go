package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/rafael/ai-pool/internal/domain"
)

// =============================================================================
// Middleware de autenticação Bearer (DB hit por request, sem cache).
//
// Spec §3.3.3 D4: revogação imediata e determinística. Sem cache positivo.
//
// Contrato: extrai o token sk-aipp-... do header Authorization, valida via
// domain.TokenValidator (HMAC-SHA256 + pepper, DB hit), e popula o contexto
// da request com o usuário e token autenticados.
//
// O handler downstream obtém o usuário via userFromContext(ctx).
// =============================================================================

// ctxKey é tipo privado para chaves de contexto (evita colisão com outros pkgs).
type ctxKey int

const (
	ctxKeyUser ctxKey = iota
	ctxKeyToken
)

// extractBearer extrai o token do header "Authorization: Bearer sk-aipp-...".
// Retorna ("", false) se ausente ou malformado.
func extractBearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// AuthMiddleware retorna um middleware que valida o Bearer token via DB hit.
//
// Em caso de falha, responde 401 no formato OpenAI e NÃO chama o próximo handler.
// O motivo do erro é comparado com sentinelas do domínio — por segurança, a
// resposta ao cliente é sempre genérica ("invalid_api_key") independentemente
// do motivo real (token inválido vs usuário desabilitado vs token expirado).
// Isto previne enumeração de tokens/usuários.
func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := extractBearer(r)
		if !ok {
			writeAuthError(w, "missing or malformed Authorization header")
			return
		}

		user, tok, err := s.tokenValidator.Validate(r.Context(), raw)
		if err != nil {
			// Resposta genérica para o cliente (não distinguir motivos).
			// Log interno com request_id já tem o detalhe (F7 SanitizingLogger).
			switch {
			case errors.Is(err, domain.ErrTokenExpired):
				// Token expirado é informação útil (cliente pode renovar).
				writeOpenAIError(w, http.StatusUnauthorized, ErrTypeAuthentication,
					"token expired", "token_expired")
			default:
				// Invalid, revoked, user disabled — mesma resposta.
				writeAuthError(w, "invalid or revoked api key")
			}
			return
		}

		// Popula o contexto para o handler downstream.
		ctx := context.WithValue(r.Context(), ctxKeyUser, user)
		ctx = context.WithValue(ctx, ctxKeyToken, tok)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// userFromContext extrai o *domain.User populado pelo AuthMiddleware.
// Panica (programmer error) se chamado fora de rota protegida — o handler NÃO
// deve ser registrado sem o AuthMiddleware.
func userFromContext(ctx context.Context) *domain.User {
	v, _ := ctx.Value(ctxKeyUser).(*domain.User)
	return v
}

// tokenFromContext extrai o *domain.APIToken populado pelo AuthMiddleware.
func tokenFromContext(ctx context.Context) *domain.APIToken {
	v, _ := ctx.Value(ctxKeyToken).(*domain.APIToken)
	return v
}
