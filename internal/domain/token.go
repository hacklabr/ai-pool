package domain

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// =============================================================================
// Tokens de API dos usuários (formato sk-aipp-...).
//
// Formato canônico (spec §3.3.3):
//
//	sk-aipp-[key_id 8 chars]-[secret 48 chars]
//	└─┘ └──┘ └──────┘ └──────┘
//	prefix projeto  público    segredo (base62-like, alta entropia)
//
// Características:
//   - Prefixo "sk-aipp-" para GitHub Secret Scanning (registro futuro).
//   - key_id é PÚBLICO (lookup O(1) por índice UNIQUE parcial em api_tokens).
//   - secret JAMAIS é persistido — apenas HMAC-SHA256(secret, pepper).
//   - pepper é injetado via env (DEV) ou arquivo readOnly (PROD, ESO/KMS).
//
// Decisões de F3:
//   - Caracteres do secret: alfanuméricos + "-" + "_" (base64 URL-safe sem padding).
//     Random bytes são codificados em base64.RawURLEncoding (sem "+/=" para
//     facilitar copy-paste e não quebrar em headers).
//   - key_id usa alfabeto restrito [A-Za-z0-9] (visível em logs/UI sem máscara).
// =============================================================================

// TokenPrefix é o prefixo obrigatório (GitHub Secret Scanning pattern).
const TokenPrefix = "sk-aipp-"

// KeyIDLength é o tamanho da parte pública do token (em bytes decodificados).
// 8 bytes base62 ≈ 47 bits de entropia — suficiente para lookup sem colisão.
const KeyIDLength = 8

// SecretLength é o tamanho do segredo (em bytes decodificados).
// 48 bytes ≈ 384 bits de entropia — inviável brute-force mesmo a milhões req/s.
const SecretLength = 48

// alphabetKeyID é o alfabeto para key_id (visível sem máscara).
const alphabetKeyID = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// ErrTokenMalformed indica formato inválido do token raw.
// Distinto de ErrTokenInvalid (definido em stores.go) — malformed é problema
// de sintaxe; invalid é lookup/HMAC que não casou.
var ErrTokenMalformed = errors.New("token malformed")

// ParsedToken contém as partes de um token sk-aipp-...
type ParsedToken struct {
	KeyID  string // 8 chars
	Secret string // 48 chars (base64-url sem padding)
}

// ParseToken valida o formato e separa key_id e secret.
//
// NÃO consulta DB nem calcula HMAC. Apenas valida sintaxe. O caller deve:
//  1. Usar key_id para lookup O(1) no DB.
//  2. Calcular DeriveTokenHMAC(secret, pepper) e comparar via hmac.Equal.
//
// Retorna ErrTokenMalformed se o formato não casa.
func ParseToken(raw string) (ParsedToken, error) {
	if !strings.HasPrefix(raw, TokenPrefix) {
		return ParsedToken{}, ErrTokenMalformed
	}
	body := raw[len(TokenPrefix):]
	parts := strings.SplitN(body, "-", 2)
	if len(parts) != 2 {
		return ParsedToken{}, ErrTokenMalformed
	}
	keyID, secret := parts[0], parts[1]
	if len(keyID) != KeyIDLength || len(secret) != SecretLength {
		return ParsedToken{}, ErrTokenMalformed
	}
	// Validação de alfabeto leve (não exaustiva — DB rejeitará qualquer key_id
	// não encontrado de qualquer forma).
	if !isASCIIAlnum(keyID) {
		return ParsedToken{}, ErrTokenMalformed
	}
	return ParsedToken{KeyID: keyID, Secret: secret}, nil
}

// DeriveTokenHMAC calcula HMAC-SHA256(secret, pepper).
//
// Defesa em profundidade (spec §3.3.3 D3): mesmo com dump do DB, atacante não
// consegue rainbow-tablear tokens sem o pepper externo.
//
// O pepper DEVE ter pelo menos 32 bytes (256 bits). Validar no caller.
// Retorno tem 32 bytes (tamanho do HMAC-SHA256).
func DeriveTokenHMAC(secret string, pepper []byte) []byte {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(secret))
	return mac.Sum(nil)
}

// GenerateToken gera um novo token criptograficamente seguro.
//
// Usa crypto/rand (NUNCA math/rand). Retorna o valor raw completo
// ("sk-aipp-...") e o HMAC correspondente. O caller persiste apenas:
//   - key_id (parte pública)
//   - token_hmac (HMAC-SHA256 com pepper)
//
// JAMAIS persistir o valor raw retornado — ele só é conhecido neste momento
// e deve ser exibido ao usuário uma única vez.
func GenerateToken(pepper []byte) (raw string, keyID string, tokenHMAC []byte, err error) {
	keyID, err = randomKeyID()
	if err != nil {
		return "", "", nil, err
	}
	secret, err := randomSecret()
	if err != nil {
		return "", "", nil, err
	}
	raw = FormatToken(keyID, secret)
	tokenHMAC = DeriveTokenHMAC(secret, pepper)
	return raw, keyID, tokenHMAC, nil
}

// FormatToken monta a string canônica a partir de partes conhecidas.
// Útil em testes.
func FormatToken(keyID, secret string) string {
	return TokenPrefix + keyID + "-" + secret
}

// MaskedToken retorna a representação segura para UI/logs:
//
//	sk-aipp-AB12CD34-••••••••
//
// JAMAIS logar o valor raw completo.
func MaskedToken(keyID string) string {
	return TokenPrefix + keyID + "-••••••••"
}

// =============================================================================
// Helpers internos de geração aleatória.
// =============================================================================

// randomKeyID gera KeyIDLength caracteres do alphabetKeyID via crypto/rand.
func randomKeyID() (string, error) {
	out := make([]byte, KeyIDLength)
	// Rejection sampling para evitar bias de módulo.
	max := byte(256 - (256 % len(alphabetKeyID)))
	for i := 0; i < KeyIDLength; i++ {
		for {
			b := make([]byte, 1)
			if _, err := rand.Read(b); err != nil {
				return "", err
			}
			if b[0] < max {
				out[i] = alphabetKeyID[int(b[0])%len(alphabetKeyID)]
				break
			}
		}
	}
	return string(out), nil
}

// randomSecret gera SecretLength bytes aleatórios codificados em base64 URL-safe
// sem padding. Como base64 produz 4 chars para cada 3 bytes, geramos bytes
// suficientes para que a codificação tenha exatamente SecretLength chars.
//
// 48 chars base64 = 36 bytes decodificados (48 / 4 * 3). Geramos 36 bytes.
func randomSecret() (string, error) {
	// SecretLength deve ser múltiplo de 4 (base64 sem padding tem tamanho
	// múltiplo de 4 com "=" removidos — RawURLEncoding produz exatamente
	// 4*ceil(n/3) chars, então para 48 chars geramos n onde 4*ceil(n/3)=48
	// → n=36).
	rawBytes := make([]byte, 36)
	if _, err := rand.Read(rawBytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(rawBytes), nil
}

// isASCIIAlnum valida se s contém apenas [A-Za-z0-9]. Quick check.
// String vazia retorna false (não é alnum).
func isASCIIAlnum(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
