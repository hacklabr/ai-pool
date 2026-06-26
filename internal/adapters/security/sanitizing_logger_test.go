package security

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestSanitizingLogger_RedactsSkAippToken(t *testing.T) {
	var buf bytes.Buffer
	handler := NewSanitizingHandler(slog.NewTextHandler(&buf, nil))
	logger := slog.New(handler)

	logger.Info("test", "token", "sk-aipp-AB12CD34-"+strings.Repeat("x", 48))

	out := buf.String()
	if strings.Contains(out, "sk-aipp-AB12CD34") {
		t.Errorf("token leaked in log: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("expected REDACTED in output: %s", out)
	}
}

func TestSanitizingLogger_RedactsPasswordAttr(t *testing.T) {
	var buf bytes.Buffer
	handler := NewSanitizingHandler(slog.NewTextHandler(&buf, nil))
	logger := slog.New(handler)

	logger.Info("test", "password", "my-super-secret-123")

	out := buf.String()
	if strings.Contains(out, "my-super-secret-123") {
		t.Errorf("password leaked: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("expected REDACTED: %s", out)
	}
}

func TestSanitizingLogger_RedactsAuthorizationAttr(t *testing.T) {
	var buf bytes.Buffer
	handler := NewSanitizingHandler(slog.NewTextHandler(&buf, nil))
	logger := slog.New(handler)

	logger.Info("test", "authorization", "Bearer some-long-token-value-here-1234567890")

	out := buf.String()
	if strings.Contains(out, "Bearer some-long-token") {
		t.Errorf("auth leaked: %s", out)
	}
}

func TestSanitizingLogger_RedactsSkAippInMessage(t *testing.T) {
	// Token embedded in message string → regex deve capturar.
	var buf bytes.Buffer
	handler := NewSanitizingHandler(slog.NewTextHandler(&buf, nil))
	logger := slog.New(handler)

	logger.Error("upstream error",
		"detail", "provider returned: invalid key sk-aipp-AB12CD34-"+strings.Repeat("x", 48),
	)

	out := buf.String()
	if strings.Contains(out, "sk-aipp-AB12CD34") {
		t.Errorf("token leaked in message attr: %s", out)
	}
}

func TestSanitizingLogger_RedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	handler := NewSanitizingHandler(slog.NewTextHandler(&buf, nil))
	logger := slog.New(handler)

	logger.Info("test",
		"kek", []byte("32-byte-kek-value-here-aaaaaaaaaa"),
		"pepper", "my-pepper-value",
		"encrypted_value", "some-encrypted-blob",
		"secret", "deepseek-key-12345",
	)

	out := buf.String()
	for _, s := range []string{"32-byte-kek", "my-pepper-value", "some-encrypted-blob", "deepseek-key-12345"} {
		if strings.Contains(out, s) {
			t.Errorf("sensitive value leaked: %q in %s", s, out)
		}
	}
}

func TestSanitizingLogger_NonSensitivePassesThrough(t *testing.T) {
	var buf bytes.Buffer
	handler := NewSanitizingHandler(slog.NewTextHandler(&buf, nil))
	logger := slog.New(handler)

	logger.Info("test",
		"model", "deepseek-chat",
		"status", 200,
		"latency_ms", 150,
	)

	out := buf.String()
	if !strings.Contains(out, "deepseek-chat") {
		t.Errorf("non-sensitive attr should pass through: %s", out)
	}
	if !strings.Contains(out, "200") {
		t.Errorf("numeric attr should pass through: %s", out)
	}
}

func TestSanitizingLogger_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	handler := NewSanitizingHandler(slog.NewTextHandler(&buf, nil))
	logger := slog.New(handler).With("api_key", "sk-test-12345678901234567890")

	logger.Info("request")

	out := buf.String()
	if strings.Contains(out, "sk-test-12345") {
		t.Errorf("api_key leaked via WithAttrs: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("expected REDACTED: %s", out)
	}
}

func TestSanitizingLogger_GroupAttrs(t *testing.T) {
	var buf bytes.Buffer
	handler := NewSanitizingHandler(slog.NewTextHandler(&buf, nil))
	logger := slog.New(handler)

	logger.Info("test", slog.Group("req",
		slog.String("path", "/v1/chat"),
		slog.String("secret", "hidden-value"),
	))

	out := buf.String()
	if strings.Contains(out, "hidden-value") {
		t.Errorf("nested secret leaked: %s", out)
	}
	if !strings.Contains(out, "/v1/chat") {
		t.Errorf("non-sensitive nested attr should pass: %s", out)
	}
}

func TestSanitizingLogger_Enabled(t *testing.T) {
	handler := NewSanitizingHandler(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if handler.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Debug should be disabled when handler is Warn-level")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Error("Error should be enabled")
	}
}
