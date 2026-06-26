// Package security implementa controles de segurança transversais (F7).
//
// SanitizingLogger: slog.Handler wrapper que redige valores sensíveis antes
// de emitir logs. Garante que chaves de provedores, tokens sk-aipp-..., KEK,
// pepper, senhas e headers de auth NUNCA apareçam em logs.
package security

import (
	"context"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// =============================================================================
// SanitizingHandler — slog.Handler que redige valores sensíveis.
// =============================================================================

// sensitiveAttrNames são nomes de atributos cujo VALOR deve ser completamente
// redigido (substituído por "REDACTED"), independentemente do conteúdo.
var sensitiveAttrNames = map[string]bool{
	"password":        true,
	"password_hash":   true,
	"secret":          true,
	"token":           true,
	"api_key":         true,
	"key":             true,
	"kek":             true,
	"pepper":          true,
	"authorization":   true,
	"encrypted_value": true,
	"decrypted_value": true,
	"decryptedvalue":  true,
	"plaintext":       true,
	"bearer":          true,
}

// redactionPatterns são regexes aplicadas a strings NÃO marcadas como sensíveis
// por nome. Detectam padrões comuns de secret embutidos em mensagens/erros.
var redactionPatterns = []*regexp.Regexp{
	// Token sk-aipp-... (qualquer comprimento após prefixo).
	regexp.MustCompile(`sk-aipp-[A-Za-z0-9_-]+`),
	// Bearer tokens genéricos (curtos demais = muitos falsos positivos;
	// exigimos 20+ chars alfanuméricos após "Bearer ").
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-+/=]{20,}`),
	// AWS access key pattern (AKIA...).
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// Google API key pattern (AIza...).
	regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),
	// Generic long alphanumeric (40+ chars) que parece ser API key.
	// Cuidado: pode ter falsos positivos em hashes; limitamos a strings com
	// prefixos comuns de API key.
	regexp.MustCompile(`(?i)(api[_-]?key|x[_-]?api[_-]?key)\s*[:=]\s*[A-Za-z0-9_\-]{20,}`),
	// Authorization header completo.
	regexp.MustCompile(`(?i)authorization\s*[:=]\s*[A-Za-z0-9._\-+/= \t]{20,}`),
}

// SanitizingHandler é um slog.Handler que aplica redação antes de delegar
// ao handler interno (JSON ou text).
type SanitizingHandler struct {
	inner slog.Handler
}

// NewSanitizingHandler envolve um slog.Handler existente.
func NewSanitizingHandler(inner slog.Handler) *SanitizingHandler {
	return &SanitizingHandler{inner: inner}
}

// Enabled delega ao handler interno.
func (h *SanitizingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle sanitiza o record antes de delegar.
func (h *SanitizingHandler) Handle(ctx context.Context, r slog.Record) error {
	// Cria um novo record com os attrs sanitizados.
	sanitized := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(attr slog.Attr) bool {
		sanitized.AddAttrs(h.sanitizeAttr(attr))
		return true
	})
	return h.inner.Handle(ctx, sanitized)
}

// WithAttrs delega sanitizando.
func (h *SanitizingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	sanitized := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		sanitized[i] = h.sanitizeAttr(a)
	}
	return &SanitizingHandler{inner: h.inner.WithAttrs(sanitized)}
}

// WithGroup delega ao handler interno.
func (h *SanitizingHandler) WithGroup(name string) slog.Handler {
	return &SanitizingHandler{inner: h.inner.WithGroup(name)}
}

// sanitizeAttr redige um attr recursivamente.
func (h *SanitizingHandler) sanitizeAttr(attr slog.Attr) slog.Attr {
	key := strings.ToLower(attr.Key)

	// 1. Nome do attr é sensível → redige valor inteiro.
	if sensitiveAttrNames[key] {
		return slog.String(attr.Key, "REDACTED")
	}

	// Resolve LogValuer (não afeta grupos ou valores primitivos).
	attr.Value = attr.Value.Resolve()

	// 2. Valor é grupo (nested attrs) → recurse ANTES de qualquer outra checagem.
	if attr.Value.Kind() == slog.KindGroup {
		groupAttrs := attr.Value.Group()
		sanitized := make([]slog.Attr, len(groupAttrs))
		for i, ga := range groupAttrs {
			sanitized[i] = h.sanitizeAttr(ga)
		}
		attr.Value = slog.GroupValue(sanitized...)
		return attr
	}

	// 3. Valor é string → aplica regex de patterns.
	if attr.Value.Kind() == slog.KindString {
		s := attr.Value.String()
		if s != "" {
			s = h.redactString(s)
			attr.Value = slog.StringValue(s)
		}
	}

	return attr
}

// redactString aplica regexes de padrões sensíveis na string.
func (h *SanitizingHandler) redactString(s string) string {
	for _, re := range redactionPatterns {
		s = re.ReplaceAllString(s, "REDACTED")
	}
	return s
}

// =============================================================================
// BuildSanitizedLogger — convenience para montar logger com SanitizingHandler.
// =============================================================================

// BuildSanitizedLogger cria um slog.Logger com SanitizingHandler envolvendo
// um handler base (JSON em prod, text em dev).
func BuildSanitizedLogger(format string, level slog.Level, w *os.File) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var base slog.Handler
	if format == "text" {
		base = slog.NewTextHandler(w, opts)
	} else {
		base = slog.NewJSONHandler(w, opts)
	}
	return slog.New(NewSanitizingHandler(base))
}
