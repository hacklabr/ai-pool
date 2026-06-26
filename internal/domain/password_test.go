package domain

import (
	"strings"
	"testing"
)

// TestHashPassword_VerifyPassword_Roundtrip valida hash + verify corretos.
func TestHashPassword_VerifyPassword_Roundtrip(t *testing.T) {
	password := []byte("supersecret-12345")
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" {
		t.Fatal("hash vazio")
	}
	// Hash Argon2id encoded tem formato "$argon2id$v=...$..."
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash não tem prefixo argon2id: %q", hash)
	}
	// Senha correta casa.
	if err := VerifyPassword(password, hash); err != nil {
		t.Errorf("VerifyPassword(correct) = %v, want nil", err)
	}
}

// TestVerifyPassword_WrongPassword rejeita senha errada.
func TestVerifyPassword_WrongPassword(t *testing.T) {
	hash, err := HashPassword([]byte("correct-12345"))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword([]byte("wrong-12345"), hash); err == nil {
		t.Error("VerifyPassword(wrong) esperava erro, teve nil")
	} else if err != ErrPasswordInvalid {
		t.Errorf("erro = %v, want ErrPasswordInvalid", err)
	}
}

// TestVerifyPassword_MalformedHash retorna ErrPasswordInvalid (não panica).
func TestVerifyPassword_MalformedHash(t *testing.T) {
	cases := []string{
		"",
		"not-a-hash",
		"$argon2id$invalid",
		"$argon2d$m=65536,t=3,p=2",
	}
	for _, h := range cases {
		if err := VerifyPassword([]byte("any-password"), h); err != ErrPasswordInvalid {
			t.Errorf("VerifyPassword(%q) = %v, want ErrPasswordInvalid", h, err)
		}
	}
}

// TestHashPassword_DifferentEachCall garante salt único por hash (mesma senha
// produz hashes diferentes).
func TestHashPassword_DifferentEachCall(t *testing.T) {
	password := []byte("same-password-12345")
	h1, _ := HashPassword(password)
	h2, _ := HashPassword(password)
	if h1 == h2 {
		t.Error("_hashes deveriam diferir (salt aleatório); se iguais, salt não está sendo gerado")
	}
	// Ambos devem verificar a mesma senha.
	if err := VerifyPassword(password, h1); err != nil {
		t.Errorf("verify h1: %v", err)
	}
	if err := VerifyPassword(password, h2); err != nil {
		t.Errorf("verify h2: %v", err)
	}
}

// TestNewFakePasswordHash produz hash válido (verificável).
// Importante: o fake hash DEVE consumir tempo similar ao hash real, caso
// contrário o timing equalization no login falha.
func TestNewFakePasswordHash(t *testing.T) {
	h := NewFakePasswordHash()
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Errorf("fake hash inválido: %q", h)
	}
	// Senha usada para gerar ("aipp-timing-equalization-placeholder") NÃO é
	// "wrongpassword" → verify deve falhar (e consumir tempo similar ao real).
	if err := VerifyPassword([]byte("wrongpassword"), h); err == nil {
		t.Error("fake hash não deveria aceitar senha arbitrária")
	}
}
