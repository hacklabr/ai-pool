package domain

import (
	"context"
	"errors"
)

// =============================================================================
// KeyResolver — PORTA do núcleo do proxy para seleção de chave por requisição.
//
// Em F3 (este commit), há UMA implementação simples: pega a primeira chave
// 'enabled' do provider que oferece o model solicitado (firstKeyResolver).
//
// Em F5 (T13-T15) será substituído pelo Routing Engine completo:
//   - LRU (memoryStore) + Afinidade (X-Conversation-ID + hash prefixo)
//   - Cadeia de failover (orquestrador stateful)
//   - Auto-disable (parser registry + scheduler)
//
// A interface é estável desde já para que a troca seja cirúrgica (substituir
// a implementação concreta injetada no proxy.Server). O handler do proxy NÃO
// conhece detalhes de LRU/afinidade/failover — apenas pede "uma chave para
// este model" e trata o erro "nenhuma disponível".
// =============================================================================

// KeyResolution é o resultado da seleção de chave para um request.
type KeyResolution struct {
	Provider Provider
	Key      ProviderKey
	// DecryptedValue é a chave de API DECODIFICADA, pronta para usar no header
	// Authorization do upstream. Vive APENAS nesta struct durante a requisição;
	// o caller DEVE zeroizá-la após uso (defer em Go via runtime: o GC eventualmente
	// libera, mas para minimizar janela de exposição em dumps, copiar para []byte
	// mutável e fill(0) é prática recomendada).
	DecryptedValue []byte
	// ViaAffinity indica se a chave foi escolhida por AFINIDADE de cache de
	// contexto (true = mesma chave da sessão/conversa → possível cache HIT no
	// provedor; false = LRU/rotação → cache MISS/frio nesta chave). Usado para
	// log "CONTEXT CACHE HIT/MISS" e header de resposta X-Context-Cache.
	ViaAffinity bool
}

// KeyResolver escolhe uma chave para atender a requisição.
//
// Recebe:
//   - model: nome do modelo solicitado pelo cliente (campo "model" do body
//     OpenAI-compatible, ex.: "deepseek-chat").
//   - orgID: organização do usuário autenticado (multi-tenant ready).
//
// Retorna:
//   - *KeyResolution com provider + key + valor decifrado, OU
//   - ErrNoKeysAvailable se nenhum provider/key elegível existe para este model.
//
// A implementação F3 não suporta exclusão (failover ainda não implementado);
// F5 estenderá o contrato para aceitar uma lista de chaves já tentadas.
type KeyResolver interface {
	ResolveKey(ctx context.Context, orgID OrganizationID, model string) (*KeyResolution, error)
}

// KeyDecryptor decifra o encrypted_value de uma ProviderKey.
//
// F3: implementação IdentityDecryptor (dev mode) assume que encrypted_value
// contém o plaintext diretamente (sem envelope). PARA DEV APENAS.
// F7: implementação AESGCMCrypto aplica envelope encryption (DEK + KEK).
//
// O contrato é: recebe a struct completa (com EncryptionKeyID para selecionar
// a KEK correta em rotação), retorna o valor plaintext.
type KeyDecryptor interface {
	Decrypt(ctx context.Context, key ProviderKey) ([]byte, error)
}

// KeyEncryptor cifra um valor plaintext e retorna o encryption_key_id +
// ciphertext. Implementado por AESGCMCrypto (F7). Usado pelo admin ao criar/
// atualizar chaves de provedores.
type KeyEncryptor interface {
	Encrypt(plaintext []byte) (encryptionKeyID string, encryptedValue []byte, err error)
}

// ErrKeyDecryptNotConfigured é retornado por IdentityDecryptor quando DevMode=false
// e nenhum decryptor real (AES-GCM) foi injetado. Em produção, NUNCA usar o
// identity decryptor.
var ErrKeyDecryptNotConfigured = errors.New("key decryption not configured for production")

// ErrModelNotFound indica que nenhum provider ativo oferece o model solicitado.
// Distinto de ErrNoKeysAvailable (provider existe mas não há chaves usable).
var ErrModelNotFound = errors.New("no active provider offers this model")
