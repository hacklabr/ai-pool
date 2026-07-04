package admin

import (
	"strings"
	"testing"
)

func TestExtractInviteToken(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/api/web/invite/abc123/accept", "abc123"},
		{"/api/web/invite/abc123/accept/", "abc123"},
		{"/api/web/invite/abc123", "abc123"},
		{"/api/web/invite/", ""},
		{"/other", ""},
	}
	for _, tc := range tests {
		got := extractInviteToken(tc.path)
		if got != tc.want {
			t.Errorf("extractInviteToken(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestHashRawInviteToken(t *testing.T) {
	h1 := hashRawInviteToken("inv-AbCdEf12-abc123")
	h2 := hashRawInviteToken("inv-AbCdEf12-abc123")
	if h1 != h2 {
		t.Error("hash should be deterministic")
	}
	h3 := hashRawInviteToken("different-token")
	if h1 == h3 {
		t.Error("different tokens should produce different hashes")
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"hello", "'hello'"},
		{"it's", "'it'\\''s'"},
		{"a'b'c", "'a'\\''b'\\''c'"},
	}
	for _, tc := range tests {
		got := shellQuote(tc.in)
		if got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderInviteScript(t *testing.T) {
	data := inviteScriptData{
		Token:     "inv-AbCdEf12-abc123",
		AcceptURL: "https://pool.example.com/api/web/invite/inv-AbCdEf12-abc123/accept",
		PoolURL:   "https://pool.example.com/api",
	}
	script, err := renderInviteScript(data)
	if err != nil {
		t.Fatalf("renderInviteScript: %v", err)
	}
	scriptStr := string(script)
	if !strings.Contains(scriptStr, "inv-AbCdEf12-abc123") {
		t.Error("script should contain token")
	}
	if !strings.Contains(scriptStr, "https://pool.example.com/api") {
		t.Error("script should contain pool URL")
	}
	if !strings.Contains(scriptStr, "llm-pool") {
		t.Error("script should configure llm-pool provider")
	}
}
