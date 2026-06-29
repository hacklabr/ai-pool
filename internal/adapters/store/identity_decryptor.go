package store

import (
	"context"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// IdentityDecryptor implementa domain.KeyDecryptor assumindo que o
// encrypted_value no DB é o PLAINTEXT direto (sem envelope encryption).
//
// USO EXCLUSIVO EM MODO DESENVOLVIMENTO (DEV_MODE=true). Em produção, NUNCA
// usar este decryptor — sempre injetar AESGCMDecryptor (F7, T21).
//
// Cenário de uso:
//   - Ambiente local com DB Docker onde o admin (em F4) ainda não aplica
//     envelope encryption, OU o DB foi populado manualmente para testes com
//     valores em plaintext.
//   - Testes de integração que não querem depender da KEK.
//
// Em produção, este decryptor retorna ErrKeyDecryptNotConfigured para qualquer
// chamada, FORÇANDO o deploy a injetar o decryptor correto.
type IdentityDecryptor struct{}

// NewIdentityDecryptor cria o decryptor dev.
func NewIdentityDecryptor() domain.KeyDecryptor {
	return IdentityDecryptor{}
}

// Decrypt retorna o encrypted_value diretamente como plaintext.
// Em F3 com dev mode, isto assume que o DB guarda plaintext.
func (IdentityDecryptor) Decrypt(_ context.Context, key domain.ProviderKey) ([]byte, error) {
	// Cópia defensiva: não devolver o slice interno do DB (evita mutação
	// acidental do cache do pgx e facilita zeroização pelo caller).
	out := make([]byte, len(key.EncryptedValue))
	copy(out, key.EncryptedValue)
	return out, nil
}
