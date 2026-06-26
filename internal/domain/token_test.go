package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"strings"
	"testing"
)

// TestParseToken_Valid valida parse de tokens bem-formados.
func TestParseToken_Valid(t *testing.T) {
	// Token canônico: 8 + 48 = 56 chars após sk-aipp-.
	keyID := "AB12CD34"
	secret := strings.Repeat("x", SecretLength)
	raw := FormatToken(keyID, secret)

	p, err := ParseToken(raw)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if p.KeyID != keyID {
		t.Errorf("KeyID = %q, want %q", p.KeyID, keyID)
	}
	if p.Secret != secret {
		t.Errorf("Secret = %q, want %q", p.Secret, secret)
	}
}

// TestParseToken_RejectsMalformed cobre casos de sintaxe inválida.
func TestParseToken_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",                       // vazio
		"sk-aipp-",               // sem body
		"sk-aipp-AB12CD34",       // sem separador "-"
		"sk-aipp-AB12CD34-",      // secret vazio
		"sk-aipp-AB1-xxx",        // keyID curto
		"sk-aipp-AB12CD34-short", // secret curto
		"sk-openai-AB12CD34-" + strings.Repeat("x", SecretLength), // prefixo errado
		"Bearer sk-aipp-AB12CD34-" + strings.Repeat("x", SecretLength),
		"xx" + FormatToken("AB12CD34", strings.Repeat("x", SecretLength)), // prefixo lixo
		FormatToken("AB!2CD34", strings.Repeat("x", SecretLength)),        // keyID com símbolo
	}
	for _, c := range cases {
		if _, err := ParseToken(c); err == nil {
			t.Errorf("ParseToken(%q) esperava erro, teve nil", c)
		} else if err != ErrTokenMalformed {
			// Aceitamos wrapped errors também, mas esperamos ErrTokenMalformed direto.
			t.Errorf("ParseToken(%q) erro = %v, want ErrTokenMalformed", c, err)
		}
	}
}

// TestDeriveTokenHMAC_MatchesRawValidates que DeriveTokenHMAC é HMAC-SHA256
// padrão com pepper como key e secret como message. Isto garante interop com
// qualquer implementação de referência.
func TestDeriveTokenHMAC_MatchesRawHMAC(t *testing.T) {
	secret := "my-test-secret"
	pepper := []byte("32-byte-pepper-aaaaaaaaaaaaaaaaaaaa!")

	got := DeriveTokenHMAC(secret, pepper)

	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(secret))
	want := mac.Sum(nil)

	if len(got) != sha256.Size {
		t.Errorf("HMAC length = %d, want %d", len(got), sha256.Size)
	}
	if string(got) != string(want) {
		t.Errorf("HMAC mismatch: got %x, want %x", got, want)
	}
}

// TestDeriveTokenHMAC_DifferentPeppers garante que pepper diferente produz
// HMAC diferente (defesa em profundidade contra dump do DB).
func TestDeriveTokenHMAC_DifferentPeppers(t *testing.T) {
	secret := "same-secret"
	p1 := DeriveTokenHMAC(secret, []byte("pepper-one-32-bytes-aaaaaaaaaaaaa!"))
	p2 := DeriveTokenHMAC(secret, []byte("pepper-two-32-bytes-bbbbbbbbbbbb!"))
	if string(p1) == string(p2) {
		t.Error("HMACs com peppers diferentes não deveriam ser iguais")
	}
}

// TestDeriveTokenHMAC_Deterministic mesmo pepper + mesmo secret = mesmo HMAC.
func TestDeriveTokenHMAC_Deterministic(t *testing.T) {
	secret := "abc"
	pepper := []byte("pepper")
	a := DeriveTokenHMAC(secret, pepper)
	b := DeriveTokenHMAC(secret, pepper)
	if string(a) != string(b) {
		t.Error("HMAC deveria ser determinístico")
	}
}

// TestGenerateToken_FormatAndPassword garante que GenerateToken produz um
// token parseável e o HMAC correspondente.
func TestGenerateToken_FormatAndPassword(t *testing.T) {
	pepper := []byte("32-bytes-of-pepper-aaaaaaaaaaaaaaaaa!")
	raw, keyID, hmacVal, err := GenerateToken(pepper)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(raw, TokenPrefix) {
		t.Errorf("raw missing prefix: %q", raw)
	}
	if len(keyID) != KeyIDLength {
		t.Errorf("keyID len = %d, want %d", len(keyID), KeyIDLength)
	}
	// Re-parse e valida roundtrip.
	p, err := ParseToken(raw)
	if err != nil {
		t.Fatalf("ParseToken(generated): %v", err)
	}
	if p.KeyID != keyID {
		t.Errorf("keyID mismatch: parsed=%q gen=%q", p.KeyID, keyID)
	}
	// HMAC esperado = DeriveTokenHMAC(secret, pepper).
	wantHMAC := DeriveTokenHMAC(p.Secret, pepper)
	if string(hmacVal) != string(wantHMAC) {
		t.Errorf("hmac mismatch: gen=%x want=%x", hmacVal, wantHMAC)
	}
}

// TestGenerateToken_Unique cada geração produz keyID diferente (probabilidade
// de colisão em 47 bits é desprezível, mas validamos empiricamente com N pequeno).
func TestGenerateToken_Unique(t *testing.T) {
	pepper := []byte("pepper")
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		_, keyID, _, err := GenerateToken(pepper)
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if seen[keyID] {
			t.Fatalf("collision on keyID %q at iter %d", keyID, i)
		}
		seen[keyID] = true
	}
}

// TestMaskedToken_NeverContainsSecret garante que a máscara NÃO vaza o secret.
// A máscara deve ter o formato "sk-aipp-XXXXXXXX-••••••••" (sem o secret real).
func TestMaskedToken_NeverContainsSecret(t *testing.T) {
	raw, keyID, _, err := GenerateToken([]byte("pepper"))
	if err != nil {
		t.Fatal(err)
	}
	m := MaskedToken(keyID)

	// 1. Máscara não deve terminar com o secret real (que está em raw após o 2º "-").
	rawParts := strings.SplitN(raw, "-", 3) // ["sk", "aipp", "KEYID-SECRET"]
	if len(rawParts) == 3 {
		secretPart := rawParts[2] // contém "KEYID-SECRET"
		// Extrai apenas o secret (depois do segundo "-").
		if idx := strings.Index(secretPart, "-"); idx >= 0 {
			realSecret := secretPart[idx+1:]
			if strings.Contains(m, realSecret) {
				t.Errorf("máscara VAZOU secret: %q contém %q", m, realSecret)
			}
		}
	}
	// 2. Deve terminar com bullets (UTF-8 multibyte).
	if !strings.HasSuffix(m, "••••••••") {
		t.Errorf("máscara deveria terminar com bullets: %q", m)
	}
	// 3. Deve conter o prefixo + keyID.
	if !strings.HasPrefix(m, TokenPrefix+keyID+"-") {
		t.Errorf("máscara deveria começar com prefixo+keyID: %q", m)
	}
}

// TestIsASCIIAlnum valida helper de alfabeto do keyID.
func TestIsASCIIAlnum(t *testing.T) {
	good := []string{"AB12cd", "ABCDef", "0", "abcDEF123"}
	bad := []string{"AB!2", "ab-cd", "AB CD", "café", ""}
	for _, s := range good {
		if !isASCIIAlnum(s) {
			t.Errorf("isASCIIAlnum(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isASCIIAlnum(s) {
			t.Errorf("isASCIIAlnum(%q) = true, want false", s)
		}
	}
}
