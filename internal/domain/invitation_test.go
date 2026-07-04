package domain

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateInvitationToken(t *testing.T) {
	tok, err := GenerateInvitationToken()
	if err != nil {
		t.Fatalf("GenerateInvitationToken failed: %v", err)
	}
	if tok == nil || tok.Raw == "" || tok.Hash == "" || tok.Prefix == "" {
		t.Fatal("expected raw, hash and prefix")
	}
	if !strings.HasPrefix(tok.Raw, InvitationPrefix) {
		t.Fatalf("token should start with %s, got %s", InvitationPrefix, tok.Raw)
	}
	if len(tok.Prefix) != InvitationPrefixLength {
		t.Fatalf("prefix length want %d, got %d", InvitationPrefixLength, len(tok.Prefix))
	}
	if !InvitationTokenEquals(tok.Raw, tok.Hash) {
		t.Fatal("InvitationTokenEquals should match generated token")
	}

	// Diferentes chamadas devem produzir tokens diferentes.
	tok2, err := GenerateInvitationToken()
	if err != nil {
		t.Fatalf("GenerateInvitationToken failed: %v", err)
	}
	if tok.Raw == tok2.Raw {
		t.Fatal("two generated tokens should differ")
	}
}

func TestInvitationTokenEquals(t *testing.T) {
	tok, err := GenerateInvitationToken()
	if err != nil {
		t.Fatalf("GenerateInvitationToken failed: %v", err)
	}
	if InvitationTokenEquals("wrong-token", tok.Hash) {
		t.Fatal("wrong token should not match")
	}
	if InvitationTokenEquals(tok.Raw, "wrong-hash") {
		t.Fatal("wrong hash should not match")
	}
}

func TestValidateInvitationPassword(t *testing.T) {
	if err := ValidateInvitationPassword("short"); err == nil {
		t.Fatal("short password should be rejected")
	}
	if err := ValidateInvitationPassword("exactly12chars"); err != nil {
		t.Fatalf("12-char password should be accepted: %v", err)
	}
	if err := ValidateInvitationPassword("a much longer secure password"); err != nil {
		t.Fatalf("long password should be accepted: %v", err)
	}
}

func TestInvitationToken_CanBeUsed(t *testing.T) {
	now := time.Now()
	pending := &InvitationToken{
		Status:    InvitationStatusPending,
		ExpiresAt: now.Add(time.Hour),
	}
	if !pending.CanBeUsed(now) {
		t.Fatal("pending non-expired token should be usable")
	}
	if pending.CanBeUsed(now.Add(2 * time.Hour)) {
		t.Fatal("expired token should not be usable")
	}

	used := &InvitationToken{Status: InvitationStatusUsed, ExpiresAt: now.Add(time.Hour)}
	if used.CanBeUsed(now) {
		t.Fatal("used token should not be usable")
	}
}
