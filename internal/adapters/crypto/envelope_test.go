package crypto

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/rafael/ai-pool/internal/domain"
)

func newTestKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand KEK: %v", err)
	}
	return kek
}

func TestAESGCM_EncryptDecryptRoundtrip(t *testing.T) {
	kek := newTestKEK(t)
	c := NewAESGCMCrypto("v1")
	if err := c.AddKEK("v1", kek); err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("sk-my-deepseek-api-key-1234567890abcdef")
	keyID, encrypted, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if keyID != "v1" {
		t.Errorf("keyID = %q, want v1", keyID)
	}
	if len(encrypted) < 12+16 {
		t.Errorf("encrypted too short: %d bytes", len(encrypted))
	}

	// Decrypt com struct ProviderKey.
	key := domain.ProviderKey{
		EncryptedValue:  encrypted,
		EncryptionKeyID: keyID,
	}
	got, err := c.Decrypt(context.Background(), key)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("roundtrip mismatch:\nwant %q\ngot  %q", plaintext, got)
	}
}

func TestAESGCM_TamperDetection(t *testing.T) {
	kek := newTestKEK(t)
	c := NewAESGCMCrypto("v1")
	_ = c.AddKEK("v1", kek)

	_, encrypted, _ := c.Encrypt([]byte("secret-value"))

	// Modifica um byte no ciphertext (não no nonce para garantir que o tag
	// não casa).
	tampered := make([]byte, len(encrypted))
	copy(tampered, encrypted)
	tampered[len(tampered)-1] ^= 0xFF // inverte último byte

	key := domain.ProviderKey{EncryptedValue: tampered, EncryptionKeyID: "v1"}
	_, err := c.Decrypt(context.Background(), key)
	if err == nil {
		t.Fatal("Decrypt should FAIL on tampered ciphertext (GCM auth)")
	}
}

func TestAESGCM_KEKRotation(t *testing.T) {
	// Cenário de rotação: KEK v1 (antiga) ainda decifra chaves velhas;
	// novas chaves são cifradas com v2 (ativa).
	kekV1 := newTestKEK(t)
	kekV2 := newTestKEK(t)

	c := NewAESGCMCrypto("v1")
	_ = c.AddKEK("v1", kekV1)

	// Cifra com v1 (ativa).
	keyID1, enc1, _ := c.Encrypt([]byte("old-secret"))

	// Rotação: registra v2 e ativa.
	_ = c.AddKEK("v2", kekV2)
	c.SetActiveKeyID("v2")

	// Cifra com v2 (nova ativa).
	keyID2, enc2, _ := c.Encrypt([]byte("new-secret"))

	if keyID1 == keyID2 {
		t.Fatal("encryption_key_id deve mudar após rotação")
	}

	// Decifra AMBAS com KEKs apropriadas.
	got1, err := c.Decrypt(context.Background(), domain.ProviderKey{
		EncryptedValue: enc1, EncryptionKeyID: keyID1,
	})
	if err != nil {
		t.Errorf("decrypt v1: %v", err)
	}
	if string(got1) != "old-secret" {
		t.Errorf("v1 roundtrip: got %q", got1)
	}

	got2, err := c.Decrypt(context.Background(), domain.ProviderKey{
		EncryptedValue: enc2, EncryptionKeyID: keyID2,
	})
	if err != nil {
		t.Errorf("decrypt v2: %v", err)
	}
	if string(got2) != "new-secret" {
		t.Errorf("v2 roundtrip: got %q", got2)
	}
}

func TestAESGCM_UnknownKEKID(t *testing.T) {
	c := NewAESGCMCrypto("v1")
	_ = c.AddKEK("v1", newTestKEK(t))

	_, err := c.Decrypt(context.Background(), domain.ProviderKey{
		EncryptedValue:  []byte("anything"),
		EncryptionKeyID: "v999",
	})
	if err == nil {
		t.Fatal("should fail for unknown KEK id")
	}
}

func TestAESGCM_EncryptWithoutKEK(t *testing.T) {
	c := NewAESGCMCrypto("v1")
	// Sem KEK registrada.
	_, _, err := c.Encrypt([]byte("test"))
	if err == nil {
		t.Fatal("Encrypt should fail without registered KEK")
	}
}

func TestAESGCM_InvalidKEKSize(t *testing.T) {
	c := NewAESGCMCrypto("v1")
	err := c.AddKEK("v1", []byte("too-short"))
	if err == nil {
		t.Fatal("should reject KEK with wrong size")
	}
}

func TestAESGCM_NonEmptyNonceUnique(t *testing.T) {
	c := NewAESGCMCrypto("v1")
	_ = c.AddKEK("v1", newTestKEK(t))

	_, enc1, _ := c.Encrypt([]byte("same-plaintext"))
	_, enc2, _ := c.Encrypt([]byte("same-plaintext"))

	// Nonces (primeiros 12 bytes) devem diferir entre encrypts.
	if string(enc1[:12]) == string(enc2[:12]) {
		t.Error("nonce reused (collision) — should be unique per Encrypt")
	}
	// Ciphertexts completos devem diferir (mesmo plaintext, nonces diferentes).
	if string(enc1) == string(enc2) {
		t.Error("ciphertext should differ for same plaintext (different nonces)")
	}
}
