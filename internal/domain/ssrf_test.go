package domain

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateProviderURL_HTTPSGood aceita URLs HTTPS válidas com hosts públicos.
// Nota: este teste faz resolução DNS real; em ambientes sem rede pode flakiar.
// Marcamos como "integration-flaky" implicitamente — se falhar por DNS, skip.
func TestValidateProviderURL_HTTPSGood(t *testing.T) {
	cases := []string{
		"https://api.openai.com/v1",
		"https://api.deepseek.com/v1/chat/completions",
		"https://generativelanguage.googleapis.com",
	}
	for _, c := range cases {
		if err := ValidateProviderURL(c); err != nil {
			// Skip em ambientes sem DNS — não é falha do código.
			if errors.Is(err, ErrSSRFHostUnresolvedable) {
				t.Logf("SKIP (no DNS): %s", c)
				continue
			}
			t.Errorf("ValidateProviderURL(%q) = %v, want nil", c, err)
		}
	}
}

// TestValidateProviderURL_BlockedSchemes rejeita schemes não-http(s).
func TestValidateProviderURL_BlockedSchemes(t *testing.T) {
	cases := []string{
		"ftp://example.com",
		"file:///etc/passwd",
		"gopher://localhost",
		"javascript:alert(1)",
		"ws://echo.example.com",
	}
	for _, c := range cases {
		err := ValidateProviderURL(c)
		if !errors.Is(err, ErrSSRFSchemeBlocked) {
			t.Errorf("ValidateProviderURL(%q) = %v, want ErrSSRFSchemeBlocked", c, err)
		}
	}
}

// TestValidateProviderURL_HTTPSOnlyByDefault rejeita http:// por padrão.
func TestValidateProviderURL_HTTPSOnlyByDefault(t *testing.T) {
	err := ValidateProviderURL("http://api.example.com")
	if !errors.Is(err, ErrSSRFSchemeBlocked) {
		t.Errorf("default mode should block http; got %v", err)
	}
}

// TestValidateProviderURL_HTTPAllowedInDev permite http:// quando configurado.
func TestValidateProviderURL_HTTPAllowedInDev(t *testing.T) {
	err := ValidateProviderURL("http://api.deepseek.com", WithSSRFAllowInsecureHTTP(true))
	if err != nil {
		// Skip se ambiente sem DNS.
		if errors.Is(err, ErrSSRFHostUnresolvedable) {
			t.Skip("no DNS in environment")
		}
		// Erro esperado é bloqueio de IP se resolveu para IP privado (não é
		// o caso de api.deepseek.com, mas pode acontecer em CI com DNS interno).
		if !errors.Is(err, ErrSSRFHostBlocked) {
			t.Errorf("got %v, want nil (or ErrSSRFHostBlocked in odd DNS env)", err)
		}
	}
}

// TestValidateProviderURL_IPLiteralsBlocked valida que IPs privados/loopback
// são rejeitados mesmo quando passados como literal.
func TestValidateProviderURL_IPLiteralsBlocked(t *testing.T) {
	cases := []string{
		"https://127.0.0.1",       // loopback
		"https://localhost",       // hostname que resolve p/ loopback
		"https://10.0.0.1",        // RFC1918
		"https://192.168.1.1",     // RFC1918
		"https://172.16.0.1",      // RFC1918
		"https://169.254.169.254", // metadata cloud (link-local)
		"https://[::1]",           // IPv6 loopback
		"https://[fe80::1]",       // IPv6 link-local
		"https://[fc00::1]",       // IPv6 ULA (privado)
		"https://0.0.0.0",         // unspecified
		"https://224.0.0.1",       // multicast
	}
	for _, c := range cases {
		err := ValidateProviderURL(c, WithSSRFAllowInsecureHTTP(true))
		if err == nil {
			t.Errorf("ValidateProviderURL(%q) esperava bloqueio, got nil", c)
			continue
		}
		// Aceitamos EITHER blocked (IP literal) ou unresolvable (hostname).
		if !errors.Is(err, ErrSSRFHostBlocked) && !errors.Is(err, ErrSSRFHostUnresolvedable) {
			t.Errorf("ValidateProviderURL(%q) = %v, want ErrSSRFHostBlocked/Unresolveable", c, err)
		}
	}
}

// TestValidateProviderURL_EmptyAndMalformed cobre edge cases de input.
func TestValidateProviderURL_EmptyAndMalformed(t *testing.T) {
	if err := ValidateProviderURL(""); err == nil {
		t.Error("empty URL should fail")
	}
	if err := ValidateProviderURL("://:"); err == nil {
		t.Error("malformed URL should fail")
	}
	if err := ValidateProviderURL("https://"); err == nil {
		t.Error("https:// with no host should fail")
	}
}

// TestIsSSRFBlocked valida helper de classificação.
func TestIsSSRFBlocked(t *testing.T) {
	if !IsSSRFBlocked(ErrSSRFSchemeBlocked) {
		t.Error("ErrSSRFSchemeBlocked should be detected")
	}
	if !IsSSRFBlocked(ErrSSRFHostBlocked) {
		t.Error("ErrSSRFHostBlocked should be detected")
	}
	if IsSSRFBlocked(ErrSSRFHostUnresolvedable) {
		t.Error("ErrSSRFHostUnresolvedable should NOT be 'blocked' (it's transient)")
	}
	// Wrapped errors também.
	wrapped := wrapErr("ctx", ErrSSRFHostBlocked)
	if !IsSSRFBlocked(wrapped) {
		t.Error("wrapped ErrSSRFHostBlocked should be detected")
	}
}

// wrapErr é um helper para validar errors.Is (wrapping com fmt.Errorf-like).
type wrappedErr struct {
	msg string
	err error
}

func (w *wrappedErr) Error() string { return w.msg + ": " + w.err.Error() }
func (w *wrappedErr) Unwrap() error { return w.err }

func wrapErr(_ string, inner error) error {
	return &wrappedErr{msg: "outer", err: inner}
}

// sanity check: garante que strings import está em uso (placeholder se necessário).
var _ = strings.Contains
