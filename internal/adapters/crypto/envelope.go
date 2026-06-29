// Package crypto implementa envelope encryption com AES-256-GCM (spec §3.3.2).
//
// KEK (Key Encryption Key) é externa ao DB — injetada via arquivo readOnly
// montado por ESO/Sealed Secrets. Suporta rotação gradual via map
// encryption_key_id → KEK. Cada chave de provedor é cifrada com a KEK ativa
// no momento da criação; o encryption_key_id persistido identifica qual KEK
// decifra.
//
// Formato do encrypted_value (BYTEA):
//
//	nonce(12 bytes) || ciphertext || tag(16 bytes embutidos no ciphertext)
//
// AES-GCM (não CBC) garante confidencialidade + integridade (auth tag).
// Tampering no ciphertext → cipher.GCM.Open retorna erro (não devolve plaintext).
package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// AESGCMCrypto implementa domain.KeyDecryptor + domain.KeyEncryptor.
// =============================================================================

// AESGCMCrypto cifra/decifra valores de chaves de provedores.
//
// KEKs: map encryption_key_id → 32-byte KEK. A KEK ativa (para Encrypt) é
// identificada por ActiveKeyID. Múltiplas KEKs coexistem para suportar
// rotação gradual: chaves antigas continuam decifrando com o id antigo enquanto
// novas são cifradas com o id novo.
type AESGCMCrypto struct {
	mu          sync.RWMutex
	keks        map[string][]byte // encryption_key_id → 32-byte KEK
	activeKeyID string
}

// NewAESGCMCrypto cria o crypto vazio. Use AddKEK para registrar KEKs.
func NewAESGCMCrypto(activeKeyID string) *AESGCMCrypto {
	if activeKeyID == "" {
		activeKeyID = "v1"
	}
	return &AESGCMCrypto{
		keks:        make(map[string][]byte),
		activeKeyID: activeKeyID,
	}
}

// AddKEK registra uma KEK com um encryption_key_id. Se o id == activeKeyID,
// esta se torna a KEK usada para novos Encrypts.
//
// A KEK DEVE ter exatamente 32 bytes (AES-256). Retorna erro se tamanho inválido.
// A KEK é armazenada em memória — NUNCA logada, NUNCA persistida no DB.
func (c *AESGCMCrypto) AddKEK(keyID string, kek []byte) error {
	if keyID == "" {
		return errors.New("crypto: empty keyID")
	}
	if len(kek) != 32 {
		return fmt.Errorf("crypto: KEK must be 32 bytes for AES-256, got %d", len(kek))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Cópia defensiva para evitar mutação externa.
	cp := make([]byte, 32)
	copy(cp, kek)
	c.keks[keyID] = cp
	return nil
}

// SetActiveKeyID muda qual KEK é usada para novos Encrypts (rotação).
// O ID deve estar registrado via AddKEK; senão, Encrypt retorna erro.
func (c *AESGCMCrypto) SetActiveKeyID(keyID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeKeyID = keyID
}

// ActiveKeyID retorna o ID da KEK ativa (para logging/debug, nunca a KEK).
func (c *AESGCMCrypto) ActiveKeyID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.activeKeyID
}

// Encrypt cifra plaintext com a KEK ativa.
//
// Retorna (encryptionKeyID, encryptedValue, error). O caller persiste ambos:
//   - encryptionKeyID → provider_keys.encryption_key_id (TEXT)
//   - encryptedValue  → provider_keys.encrypted_value (BYTEA)
//
// Formato do encryptedValue:
//
//	nonce(12 bytes) || ciphertext+tag (GCM.Seal anexa tag ao ciphertext)
//
// Nonce é gerado com crypto/rand (12 bytes por GCM spec). NUNCA reusar nonce
// com a mesma KEK — random 12 bytes dá probabilidade de colisão desprezível.
func (c *AESGCMCrypto) Encrypt(plaintext []byte) (string, []byte, error) {
	c.mu.RLock()
	keyID := c.activeKeyID
	kek, ok := c.keks[keyID]
	c.mu.RUnlock()
	if !ok {
		return "", nil, fmt.Errorf("crypto: active KEK %q not registered", keyID)
	}

	gcm, err := newGCM(kek)
	if err != nil {
		return "", nil, fmt.Errorf("crypto: newGCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, fmt.Errorf("crypto: rand nonce: %w", err)
	}

	// Seal(dst, nonce, plaintext, additionalData) → nonce+ciphertext+tag.
	// dst = nonce para que o output seja nonce || ciphertext || tag em uma slice.
	encrypted := gcm.Seal(nonce, nonce, plaintext, nil)
	return keyID, encrypted, nil
}

// Decrypt decifra o encrypted_value de uma ProviderKey.
//
// Usa key.EncryptionKeyID para selecionar a KEK. Se o id não está registrado
// (ex.: KEK foi removida sem re-encriptar as chaves), retorna erro claro.
//
// Tampering no ciphertext → GCM.Open retorna erro (auth tag mismatch).
// NUNCA devolve plaintext parcial em caso de falha.
func (c *AESGCMCrypto) Decrypt(_ context.Context, key domain.ProviderKey) ([]byte, error) {
	c.mu.RLock()
	kek, ok := c.keks[key.EncryptionKeyID]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("crypto: KEK %q not registered (rotation incomplete?)", key.EncryptionKeyID)
	}

	gcm, err := newGCM(kek)
	if err != nil {
		return nil, fmt.Errorf("crypto: newGCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(key.EncryptedValue) < nonceSize {
		return nil, fmt.Errorf("crypto: encrypted value too short (got %d, need >= %d)",
			len(key.EncryptedValue), nonceSize)
	}

	nonce := key.EncryptedValue[:nonceSize]
	ciphertext := key.EncryptedValue[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: GCM authentication failed (tampered or wrong KEK): %w", err)
	}
	// Cópia defensiva — o slice retornado por GCM.Open é alocado internamente,
	// mas copiamos para garantir que o caller possua uma slice independente
	// (que pode ser zeroizada após uso sem afetar caches internos).
	out := make([]byte, len(plaintext))
	copy(out, plaintext)
	return out, nil
}

// LoadKEKFromFile carrega uma KEK de um arquivo. O arquivo deve conter
// exatamente 32 bytes raw (não base64 — para evitar ambiguidade).
//
// Em produção, o arquivo é montado via ESO/Sealed Secrets como readOnly.
// Retorna (kek32bytes, error).
func LoadKEKFromFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("crypto: empty KEK path")
	}
	kek, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("crypto: read KEK file %q: %w", path, err)
	}
	if len(kek) != 32 {
		return nil, fmt.Errorf("crypto: KEK file %q has %d bytes, expected 32", path, len(kek))
	}
	return kek, nil
}

// newGCM cria um cipher.AEAD (GCM) a partir de uma KEK de 32 bytes.
func newGCM(kek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
