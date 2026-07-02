package postgres

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TokenValidator implementa domain.TokenValidator com DB hit por request
// (spec §3.3.3 D4 — sem cache positivo).
//
// Pipeline do Validate (chamado no middleware de auth a CADA request):
//
//  1. ParseToken(raw) → keyID, secret. (sintaxe; sem DB)
//  2. tokenRepo.GetActiveByKeyID(keyID) → *APIToken. (DB hit O(1) por partial index)
//  3. DeriveTokenHMAC(secret, pepper) → calculado localmente.
//  4. subtle.ConstantTimeCompare(storedHMAC, computedHMAC) → constant-time.
//  5. userRepo.GetByID(token.UserID) → checar status (revogação imediata).
//  6. token.IsValid(now) → expiração.
//
// Erros convertidos para sentinelas do domínio (comparáveis com errors.Is):
//   - ErrTokenMalformed → ErrTokenInvalid (sintaxe ruim).
//   - DB ErrNotFound    → ErrTokenInvalid (key_id inexistente).
//   - HMAC não casa     → ErrTokenInvalid (token forjado ou key_id reciclado).
//   - User desabilitado → ErrTokenRevoked (revogação imediata via DB).
//   - Token expirado    → ErrTokenExpired.
//
// Importante (Security): NÃO distinguir "token inválido" de "usuário
// desabilitado" na resposta ao cliente (timing/enumeration). Internamente
// sabemos; a resposta HTTP é 401 genérico. O logger interno registra o motivo.
type TokenValidator struct {
	tokenRepo *TokenRepo
	userRepo  *UserRepo
	pepper    []byte
}

// NewTokenValidator cria o validador. pepper DEVE ter pelo menos 32 bytes.
// O caller é responsável por carregar o pepper (env em dev, arquivo em prod).
func NewTokenValidator(pool *pgxpool.Pool, pepper []byte) *TokenValidator {
	return &TokenValidator{
		tokenRepo: NewTokenRepo(pool),
		userRepo:  NewUserRepo(pool),
		pepper:    pepper,
	}
}

// Validate implementa domain.TokenValidator.
//
// DB hit por request: a partial unique index em api_tokens(key_id) WHERE
// revoked_at IS NULL torna o lookup sub-ms. Ver spec §3.3.3.
func (v *TokenValidator) Validate(ctx context.Context, rawToken string) (*domain.User, *domain.APIToken, error) {
	// 1. Parse sintático.
	parsed, err := domain.ParseToken(rawToken)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrTokenInvalid, err)
	}

	// 2. Lookup DB pelo key_id (somente tokens ativos e não-revogados).
	tok, err := v.tokenRepo.GetActiveByKeyID(ctx, parsed.KeyID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, domain.ErrTokenInvalid
		}
		return nil, nil, fmt.Errorf("token validator: token lookup: %w", err)
	}

	// 3. Calcula HMAC do secret enviado pelo cliente.
	computed := domain.DeriveTokenHMAC(parsed.Secret, v.pepper)

	// 4. Comparação constant-time. subtle.ConstantTimeCompare previne timing
	// attack. Retorna 0 para tamanhos diferentes — também constant-time em
	// relação ao tamanho (não revela o comprimento do HMAC armazenado).
	if !hmacEqual(tok.TokenHMAC, computed) {
		// Token forjado OU key_id foi reciclado após revogação (parcial unique
		// index só garante unicidade entre ativos). Tratar como inválido.
		return nil, nil, domain.ErrTokenInvalid
	}

	// 5. Carrega o usuário para checar status (revogação imediata).
	user, err := v.userRepo.GetByID(ctx, tok.OrgID, tok.UserID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Usuário foi fisicamente removido — tratar como revogado.
			return nil, nil, domain.ErrTokenRevoked
		}
		return nil, nil, fmt.Errorf("token validator: user lookup: %w", err)
	}

	// 6. Status do usuário: desabilitado = revogado (spec §3.3.3).
	if !user.CanAccessPool() {
		return nil, nil, domain.ErrTokenRevoked
	}

	// 7. Validade do token (expiração).
	now := time.Now().UTC()
	if !tok.IsValid(now) {
		if tok.ExpiresAt != nil && now.After(*tok.ExpiresAt) {
			return nil, nil, domain.ErrTokenExpired
		}
		return nil, nil, domain.ErrTokenRevoked
	}

	return user, tok, nil
}

// hmacEqual compara dois HMACs em tempo constante. Retorna false se tamanhos
// diferem (também constant-time em relação ao tamanho).
func hmacEqual(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}
