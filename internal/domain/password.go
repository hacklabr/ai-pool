package domain

import (
	"errors"

	"github.com/alexedwards/argon2id"
)

// =============================================================================
// Hash de senhas — Argon2id (spec §3.3.5).
//
// Argon2id é o vencedor da Password Hashing Competition 2015. Resistente a
// GPU/ASIC. A lib alexedwards/argon2id implementa o formato encoded (similar
// ao do Django) — string única com salt+params+hash, portátil entre instâncias.
//
// Parâmetros default da lib (memory=64MB, iterations=3, parallelism=2) são
// calibrados para server-side com ~1s de compute por hash. Adequados para login
// web admin (~30 usuários internos).
//
// NUNCA usar para tokens sk-aipp-... (função lenta; tokens de alta entropia
// não precisam de Argon2 — DeriveTokenHMAC com pepper basta).
// =============================================================================

// ErrPasswordInvalid indica que a senha não casa o hash. Sinal genérico —
// nunca revelar se foi senha errada vs usuário inexistente (anti-enumeration).
var ErrPasswordInvalid = errors.New("invalid credentials")

// HashPassword calcula o hash Argon2id de uma senha em plaintext.
//
// Retorna string encoded (portátil: contém salt+params+hash). Persistir no DB
// em TEXT (campo users.password_hash).
//
// A senha em plaintext DEVE ser zeroizada pelo caller após a chamada (Go não
// permite zeroizar strings imutáveis diretamente — caller deve usar []byte).
//
// Em erro de rand (extremamente raro), retorna erro wrapped.
func HashPassword(password []byte) (string, error) {
	// CreateHash usa os params default da lib (ID, memory, iterations, parallelism,
	// saltLength=16, keyLength=32). Estes são calibrados para OWASP 2023.
	hash, err := argon2id.CreateHash(string(password), argon2id.DefaultParams)
	if err != nil {
		return "", err
	}
	return hash, nil
}

// VerifyPassword compara senha em plaintext com o hash Argon2id encoded.
//
// Usa argon2id.ComparePasswordAndHash que internamente faz comparação
// constant-time do hash derivado. Retorna:
//   - nil se senha casa.
//   - ErrPasswordInvalid se senha NÃO casa (ou hash malformado — tratamos
//     igualmente para não distinguir "hash inválido" de "senha errada").
//
// Anti-enumeration: o caller DEVE chamar HashPassword/VerifyPassword com timing
// similar para usuários existentes vs inexistentes. Para usuários inexistentes,
// use um hash fake fixo (placeholder) e descarte o resultado — ver login.go.
func VerifyPassword(password []byte, hash string) error {
	match, err := argon2id.ComparePasswordAndHash(string(password), hash)
	if err != nil {
		// Hash malformado ou params inválidos. Tratar como senha inválida.
		return ErrPasswordInvalid
	}
	if !match {
		return ErrPasswordInvalid
	}
	return nil
}

// FakePasswordHash é um hash Argon2id válido usado em timing equalization:
// quando procuramos um usuário inexistente no login, comparamos a senha
// enviada contra este hash fake e descartamos o resultado. Isto garante tempo
// similar entre "usuário existe com senha errada" e "usuário não existe",
// evitando timing attack para enumeração de usuários.
//
// Gerado uma vez no boot (HashPassword("dummy")) e reusado. Custo da geração
// é pago 1x; a verificação subsequente é o que importa para timing.
func NewFakePasswordHash() string {
	h, _ := HashPassword([]byte("aipp-timing-equalization-placeholder"))
	return h
}
