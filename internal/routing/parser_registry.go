package routing

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// Alias de conveniência para o tipo que vive em domain.
type ErrorParser = domain.ErrorParser

// =============================================================================
// ParserRegistry + DefaultErrorParser — extração de data de liberação de cota
// de mensagens de erro dos provedores.
//
// Cada provedor tem formato próprio. Implementamos DefaultErrorParser com
// regex genérico cobrindo padrões comuns (ISO 8601, RFC 3339, epoch unix,
// "in N seconds/minutes/hours"). Para provedores específicos, podem ser
// registrados parsers customizados que herdam/sobrescrevem o default.
// =============================================================================

// ParserRegistry mantém ErrorParser por slug de provedor.
//
// Lookup: slug → parser registrado; se não há, cai para Default.
// Adicionar parser de novo provedor = Register("newslug", impl).
type ParserRegistry struct {
	parsers map[string]ErrorParser
	def     ErrorParser
}

// NewParserRegistry cria registry vazio com DefaultErrorParser como fallback.
func NewParserRegistry() *ParserRegistry {
	return &ParserRegistry{
		parsers: make(map[string]ErrorParser),
		def:     &DefaultErrorParser{},
	}
}

// Register adiciona/substitui um parser para um slug de provedor.
// Idempotente.
func (r *ParserRegistry) Register(providerSlug string, p ErrorParser) {
	r.parsers[strings.ToLower(providerSlug)] = p
}

// ParseResetDate despacha para o parser apropriado (ou default).
func (r *ParserRegistry) ParseResetDate(providerSlug string, statusCode int, body []byte) (time.Time, bool) {
	if p, ok := r.parsers[strings.ToLower(providerSlug)]; ok {
		return p.ParseResetDate(statusCode, body)
	}
	return r.def.ParseResetDate(statusCode, body)
}

// DefaultParser retorna o parser default (para uso direto se necessário).
func (r *ParserRegistry) DefaultParser() ErrorParser { return r.def }

// =============================================================================
// DefaultErrorParser — regex genérico para formatos comuns.
// =============================================================================

// DefaultErrorParser cobre padrões de data mais comuns em mensagens de erro
// de provedores de LLM. Não é específico; funciona razoavelmente para maioria.
type DefaultErrorParser struct{}

// regexes compiladas uma vez (imutáveis; thread-safe após MustCompile).
var (
	// ISO 8601 / RFC 3339: 2026-12-31T23:59:59Z | 2026-12-31T23:59:59+00:00
	reISO8601 = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2}))`)
	// Data simples: 2026-12-31 (sem hora) — assume meia-noite UTC.
	reDateOnly = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	// Unix epoch (10 dígitos, segundos): 1735689599
	reUnixEpoch = regexp.MustCompile(`\b1[0-9]{9}\b`)
	// Relativo: "in 60 seconds" / "in 5 minutes" / "in 2 hours" / "in 1 day"
	reRelativeSec = regexp.MustCompile(`(?i)in\s+(\d+)\s+seconds?`)
	reRelativeMin = regexp.MustCompile(`(?i)in\s+(\d+)\s+minutes?`)
	reRelativeHr  = regexp.MustCompile(`(?i)in\s+(\d+)\s+hours?`)
	reRelativeDay = regexp.MustCompile(`(?i)in\s+(\d+)\s+days?`)
	// "retry after N" (às vezes lowercase, às vezes em JSON com aspas): similar a relativo.
	// Captura "retry-after": 30, retry-after: 30, retry_after=30, etc.
	reRetryAfter = regexp.MustCompile(`(?i)retry[-_ ]?after["']?\s*[:=]\s*(\d+)`)
)

// ParseResetDate implementa ErrorParser.
//
// Estratégia (primeiro match vence):
//  1. ISO 8601 / RFC 3339 (mais específico).
//  2. Retry-After: N (segundos).
//  3. "in N seconds/minutes/hours/days".
//  4. Unix epoch (10 digits).
//  5. Date-only "YYYY-MM-DD" (assume meia-noite UTC).
//
// Retorna (zero, false) se nada casa.
func (p *DefaultErrorParser) ParseResetDate(_ int, body []byte) (time.Time, bool) {
	bodyStr := string(body)
	now := time.Now().UTC()

	// 1. ISO 8601 / RFC 3339.
	if m := reISO8601.FindString(bodyStr); m != "" {
		// Tenta layouts comuns.
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05Z",
			"2006-01-02T15:04:05",
		} {
			if t, err := time.Parse(layout, m); err == nil {
				return t.UTC(), true
			}
		}
	}

	// 2. Retry-After: N (segundos).
	if m := reRetryAfter.FindStringSubmatch(bodyStr); len(m) >= 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n < 86400*7 {
			return now.Add(time.Duration(n) * time.Second), true
		}
	}

	// 3. "in N seconds/minutes/hours/days".
	if t, ok := matchRelative(bodyStr, now); ok {
		return t, true
	}

	// 4. Unix epoch.
	if m := reUnixEpoch.FindString(bodyStr); m != "" {
		if n, err := strconv.ParseInt(m, 10, 64); err == nil {
			t := time.Unix(n, 0).UTC()
			// Sanity: data deve estar no futuro (próximos 30 dias) ou recente.
			if t.After(now) && t.Before(now.Add(30*24*time.Hour)) {
				return t, true
			}
		}
	}

	// 5. Date-only.
	if m := reDateOnly.FindStringSubmatch(bodyStr); len(m) >= 4 {
		t, err := time.Parse("2006-01-02", m[1]+"-"+m[2]+"-"+m[3])
		if err == nil {
			t = t.UTC()
			if t.After(now) && t.Before(now.Add(365*24*time.Hour)) {
				return t, true
			}
		}
	}

	return time.Time{}, false
}

// matchRelative busca padrões "in N units" e retorna o timestamp futuro.
func matchRelative(s string, now time.Time) (time.Time, bool) {
	if m := reRelativeSec.FindStringSubmatch(s); len(m) >= 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return now.Add(time.Duration(n) * time.Second), true
		}
	}
	if m := reRelativeMin.FindStringSubmatch(s); len(m) >= 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return now.Add(time.Duration(n) * time.Minute), true
		}
	}
	if m := reRelativeHr.FindStringSubmatch(s); len(m) >= 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return now.Add(time.Duration(n) * time.Hour), true
		}
	}
	if m := reRelativeDay.FindStringSubmatch(s); len(m) >= 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return now.AddDate(0, 0, n), true
		}
	}
	return time.Time{}, false
}

// =============================================================================
// Stubs para provedores conhecidos (gemini, mistral, deepseek, zai, kimi).
// Por ora herdam do DefaultErrorParser; parsers específicos podem ser
// adicionados conforme coletamos fixtures de erros reais.
// =============================================================================

// RegisterKnownProviders registra stubs para provedores comuns. Cada um herda
// o comportamento do DefaultErrorParser — refinamentos específicos podem ser
// feitos conforme coletamos amostras de erros reais de cada provedor.
//
// Para substituir um parser específico, chame registry.Register(slug, impl)
// com sua implementação customizada APÓS esta função.
func RegisterKnownProviders(r *ParserRegistry) {
	for _, slug := range []string{"gemini", "mistral", "deepseek", "zai", "z-ai", "kimi", "moonshot"} {
		r.Register(slug, &DefaultErrorParser{})
	}
}
